package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/docker/compose/v5/pkg/remote"
)

// composeBackend is the in-process Docker Compose v5 SDK binding. Compose ops
// (up/down/pull/restart/recreate) run as Go library calls over the same docker
// socket the fleet uses — NO `docker compose` subprocess, no stdout scraping.
// One command.Cli is built at startup and reused;
// each mutating op gets its OWN api.Compose instance so progress + stream output
// route into that job's log ring (compose binds the EventProcessor/streams at
// construction, so they can't be swapped per-call on a shared instance).
//
// The cli holds its own moby client over the socket — a second client object
// from the one fleet.go/dockerclient.go use (github.com/docker/docker/client).
// Both talk to the same socket; nothing is shared, so the two client libraries
// (docker/cli v29's moby/moby/client and docker/docker v28) coexist cleanly.
type composeBackend struct {
	cli  command.Cli
	base api.Compose // no per-job streams — used for LoadProject / queries only
	root string      // ComposeRoot: every file a load reads must resolve under it
	// modelCheck is the pre-load scan (preLoadCheck). The real load
	// guards include/extends on its own as well — the two build their options
	// separately, so each must hold without the other; a test swaps this out to
	// prove it.
	modelCheck func(ctx context.Context, e ProjectEntry, paths loadPaths, guard *loadGuard) error
}

func newComposeBackend(cfg Config) (*composeBackend, error) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		return nil, fmt.Errorf("new docker cli: %w", err)
	}
	opts := cliflags.NewClientOptions()
	if cfg.DockerHost != "" {
		opts.Hosts = []string{cfg.DockerHost}
	}
	// Private-registry pulls read auth from the mounted config.json;
	// its directory is the docker config dir. Harmless when unset.
	if cfg.RegistryAuthFile != "" {
		opts.ConfigDir = filepath.Dir(cfg.RegistryAuthFile)
	}
	if err := dockerCli.Initialize(opts); err != nil {
		return nil, fmt.Errorf("init docker cli: %w", err)
	}
	base, err := compose.NewComposeService(dockerCli, compose.WithPrompt(compose.AlwaysOkPrompt()))
	if err != nil {
		return nil, fmt.Errorf("compose service: %w", err)
	}
	b := &composeBackend{cli: dockerCli, base: base, root: cfg.ComposeRoot}
	b.modelCheck = b.preLoadCheck
	return b, nil
}

// serviceForJob returns an api.Compose whose progress events + Out/Err stream
// writes are funneled into the given job's log ring (and fleet-nudged on change).
func (b *composeBackend) serviceForJob(j *Job, fleetTrigger func()) (api.Compose, error) {
	w := &jobLineWriter{job: j}
	return compose.NewComposeService(b.cli,
		compose.WithPrompt(compose.AlwaysOkPrompt()),
		compose.WithEventProcessor(&jobEventProcessor{job: j, fleetTrigger: fleetTrigger, last: map[string]string{}}),
		compose.WithOutputStream(w),
		compose.WithErrorStream(w),
	)
}

// execute runs one project-scoped compose op via a per-job api.Compose so all
// progress + stream output lands in the job log. down/restart act by project
// name off the live containers; up/recreate/pull need the loaded project model.
func (b *composeBackend) execute(ctx context.Context, j *Job, op string, e ProjectEntry, fleetTrigger func()) error {
	svc, err := b.serviceForJob(j, fleetTrigger)
	if err != nil {
		return fmt.Errorf("compose service: %w", err)
	}

	switch op {
	case opComposeDown:
		// Safe default: stop + remove containers/networks, but NEVER volumes.
		return svc.Down(ctx, e.Name, api.DownOptions{RemoveOrphans: true})
	case opComposeRestart:
		return svc.Restart(ctx, e.Name, api.RestartOptions{Timeout: jobTimeoutDur(j)})
	}

	project, err := b.loadProject(ctx, e)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	switch op {
	case opComposeUp:
		// Detached `up` (create + start, return after start) — the deploy
		// posture. Health-wait + rollback is the `update` op (stackengine.go).
		return svc.Up(ctx, project, api.UpOptions{
			Create: api.CreateOptions{RemoveOrphans: true},
			Start:  api.StartOptions{Project: project},
		})
	case opComposeRecreate:
		return svc.Up(ctx, project, api.UpOptions{
			Create: api.CreateOptions{Recreate: api.RecreateForce, RemoveOrphans: true},
			Start:  api.StartOptions{Project: project},
		})
	case opComposePull:
		return svc.Pull(ctx, project, api.PullOptions{})
	}
	return fmt.Errorf("unknown compose op %q", op)
}

// pullProject pulls the images for an ALREADY-LOADED (and possibly image-mutated)
// project via a per-job compose service, so progress lands in the job log. Used
// by the update engine, which mutates project.Services[*].Image
// in-memory before deploying — it must NOT reload from disk (loadProject) or the
// resolver's target tags would be lost.
func (b *composeBackend) pullProject(ctx context.Context, j *Job, project *types.Project, fleetTrigger func()) error {
	svc, err := b.serviceForJob(j, fleetTrigger)
	if err != nil {
		return fmt.Errorf("compose service: %w", err)
	}
	return svc.Pull(ctx, project, api.PullOptions{IgnoreFailures: false})
}

// upProject runs `up` (create+start) on an already-loaded project with the given
// recreate strategy (api.RecreateForce for an update/rollback). Pull is governed
// by each service's PullPolicy on the project model (the engine sets "never" on
// rollback so it redeploys the snapshot image IDs without re-pulling).
func (b *composeBackend) upProject(ctx context.Context, j *Job, project *types.Project, recreate string, fleetTrigger func()) error {
	svc, err := b.serviceForJob(j, fleetTrigger)
	if err != nil {
		return fmt.Errorf("compose service: %w", err)
	}
	return svc.Up(ctx, project, api.UpOptions{
		Create: api.CreateOptions{Recreate: recreate, RemoveOrphans: true, QuietPull: true},
		Start:  api.StartOptions{Project: project},
	})
}

// jobTimeoutDur converts the job's optional timeout (seconds) to the
// *time.Duration the compose options expect; nil = engine/image default.
func jobTimeoutDur(j *Job) *time.Duration {
	if j.timeout == nil {
		return nil
	}
	d := time.Duration(*j.timeout) * time.Second
	return &d
}

// loadProject parses + validates a project's compose files into a typed model.
// Uses the base service (no per-job stream wiring needed for a pure load).
//
// Every file the load reads must resolve under ComposeRoot, and a refusal replaces
// compose's own error text — which could quote a file it read (projectfiles.go):
//
//  1. resolveLoadPaths decides the compose and env files, and exactly those are
//     passed: compose's discovery (COMPOSE_FILE, default names, the override
//     file, the parent-directory walk) never runs. Each is confined first.
//  2. A loadGuard sits in compose's resource-loader chain ahead of its local
//     loader, and refuses an `include` or `extends.file` that escapes the root
//     before the file is read. It also watches include events, so an included
//     project's .env is checked before compose loads it.
//  3. Pre-load scans (modelCheck), for what compose reads with no hook at all:
//     every `include:` entry's paths, env_file and project_directory, recursively
//     (includescan.go), then — from the raw model, loaded with includes and extends
//     applied but no label or env file read — every service `label_file`. Include
//     processing and label_file loading both read files, and compose's dotenv
//     parser quotes a line it cannot parse.
//  4. Service environment resolution — reading every service `env_file` — is
//     deferred. After the load, every service env_file and label_file in the
//     project is confined, and only then is the environment resolved.
func (b *composeBackend) loadProject(ctx context.Context, e ProjectEntry) (*types.Project, error) {
	paths, err := resolveLoadPaths(e)
	if err != nil {
		return nil, err
	}
	for _, p := range paths.all() {
		if err := confinePath(p, b.root); err != nil {
			return nil, err
		}
	}
	guard := newLoadGuard(b.root, e.WorkingDir, b.remoteLoaders())
	if err := b.modelCheck(ctx, e, paths, guard); err != nil {
		return nil, err
	}
	project, err := b.base.LoadProject(ctx, api.ProjectLoadOptions{
		ProjectName: e.Name,
		ConfigPaths: paths.config,
		WorkingDir:  e.WorkingDir,
		EnvFiles:    paths.env,
		Profiles:    e.Profiles,
		ProjectOptionsFns: []cli.ProjectOptionsFn{
			cli.WithResourceLoader(guard),
			cli.WithoutEnvironmentResolution,
		},
		LoadListeners: []api.LoadListener{guard.listen},
	})
	if v := guard.violation(); v != nil {
		return nil, v // compose's error, if any, may quote the refused file
	}
	if err != nil {
		return nil, err
	}
	for _, svc := range project.Services {
		for _, ef := range svc.EnvFiles {
			if err := confinePath(absAgainst(project.WorkingDir, ef.Path), b.root); err != nil {
				return nil, err
			}
		}
		for _, lf := range svc.LabelFiles {
			if err := confinePath(absAgainst(project.WorkingDir, lf), b.root); err != nil {
				return nil, err
			}
		}
	}
	return project.WithServicesEnvironmentResolved(false)
}

// preLoadCheck confines what compose reads with no hook, before any load does: the
// include tree (includescan.go), then every service label_file in the raw model.
// Both run with the same files and environment as the real load.
func (b *composeBackend) preLoadCheck(ctx context.Context, e ProjectEntry, paths loadPaths, guard *loadGuard) error {
	fns := []cli.ProjectOptionsFn{
		cli.WithWorkingDirectory(e.WorkingDir),
		cli.WithOsEnv,
		cli.WithEnvFiles(paths.env...),
		cli.WithDotEnv,
		cli.WithName(e.Name),
		cli.WithResourceLoader(guard),
		cli.WithLoadOptions(func(o *loader.Options) { o.Listeners = append(o.Listeners, guard.listen) }),
	}
	for _, r := range b.remoteLoaders() {
		fns = append(fns, cli.WithResourceLoader(r))
	}
	opts, err := cli.NewProjectOptions(paths.config, fns...)
	if err != nil {
		return err
	}
	scan := includeScan{root: b.root, remotes: b.remoteLoaders(), onProjectDir: guard.addBase}
	if err := scan.scan(ctx, paths.config, e.WorkingDir, e.WorkingDir, opts.Environment, 0, nil); err != nil {
		return err
	}
	model, err := opts.LoadModel(ctx)
	if v := guard.violation(); v != nil {
		return v
	}
	if err != nil {
		return err
	}
	services, _ := model["services"].(map[string]any)
	for _, raw := range services {
		svc, _ := raw.(map[string]any)
		var files []any
		switch v := svc["label_file"].(type) {
		case []any:
			files = v
		case string:
			files = []any{v}
		}
		for _, f := range files {
			path, ok := f.(string)
			if !ok {
				continue
			}
			if err := confinePath(absAgainst(e.WorkingDir, path), b.root); err != nil {
				return err
			}
		}
	}
	return nil
}

// remoteLoaders are compose's own git and OCI loaders: a reference either accepts
// is remote content, not a local file, and not the guard's to judge.
func (b *composeBackend) remoteLoaders() []loader.ResourceLoader {
	if b.cli == nil {
		return nil
	}
	return []loader.ResourceLoader{
		remote.NewGitRemoteLoader(b.cli, false),
		remote.NewOCIRemoteLoader(b.cli, false, api.OCIOptions{}),
	}
}

// loadGuard is a compose-go ResourceLoader placed ahead of the local loader. It
// accepts only a LOCAL reference that escapes the compose root and fails its load,
// so compose never opens it; every other reference falls through to compose's own
// loaders unchanged.
//
// A relative reference is resolved by compose against the directory of the load
// context it appears in — the project's, or an included project's — which the
// loader interface does not pass. The guard therefore checks it against every such
// directory it knows and refuses if any resolution escapes. It knows the project's
// working dir, every included project's directory from the include scan, and — on
// its own, for a top-level include — the directory an include event names. A
// nested include's event carries a directory RELATIVE to its includer, which is
// never used as a base: resolving against it would refuse legitimate files.
type loadGuard struct {
	root    string
	remotes []loader.ResourceLoader

	mu    sync.Mutex
	bases map[string]struct{}
	err   error
}

func newLoadGuard(root, workingDir string, remotes []loader.ResourceLoader) *loadGuard {
	return &loadGuard{root: root, remotes: remotes, bases: map[string]struct{}{filepath.Clean(workingDir): {}}}
}

func (g *loadGuard) addBase(dir string) {
	if !filepath.IsAbs(dir) {
		return
	}
	g.mu.Lock()
	g.bases[filepath.Clean(dir)] = struct{}{}
	g.mu.Unlock()
}

func (g *loadGuard) violation() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *loadGuard) refuse(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err == nil {
		g.err = err
	}
}

func (g *loadGuard) isRemote(ref string) bool {
	for _, r := range g.remotes {
		if r.Accept(ref) {
			return true
		}
	}
	return false
}

// escape returns the refusal for a local reference, or nil when every resolution
// stays under the root.
func (g *loadGuard) escape(ref string) error {
	if filepath.IsAbs(ref) {
		return confinePath(ref, g.root)
	}
	g.mu.Lock()
	bases := make([]string, 0, len(g.bases))
	for b := range g.bases {
		bases = append(bases, b)
	}
	g.mu.Unlock()
	for _, b := range bases {
		if err := confinePath(filepath.Join(b, ref), g.root); err != nil {
			return err
		}
	}
	return nil
}

func (g *loadGuard) Accept(ref string) bool {
	if g.isRemote(ref) {
		return false
	}
	if err := g.escape(ref); err != nil {
		g.refuse(err)
		return true
	}
	return false
}

func (g *loadGuard) Load(_ context.Context, ref string) (string, error) {
	if err := g.violation(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("refusing to load %s", echo(ref))
}

func (g *loadGuard) Dir(ref string) string { return filepath.Dir(ref) }

// listen records each include's working dir (and the included project's
// directory) as a base for later relative references, and checks the included
// project's .env — which compose reads without consulting any loader.
func (g *loadGuard) listen(event string, metadata map[string]any) {
	if event != "include" {
		return
	}
	wd, _ := metadata["workingdir"].(string)
	var refs []string
	switch v := metadata["path"].(type) {
	case types.StringList:
		refs = v
	case []string:
		refs = v
	}
	if !filepath.IsAbs(wd) || len(refs) == 0 || g.isRemote(refs[0]) {
		return // a nested include: its directories come from the include scan
	}
	projectDir := filepath.Dir(absAgainst(wd, refs[0]))
	g.addBase(projectDir)
	dotenv := filepath.Join(projectDir, ".env")
	if _, err := os.Lstat(dotenv); err == nil {
		if err := confinePath(dotenv, g.root); err != nil {
			g.refuse(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Progress capture: compose's EventProcessor → the job log ring.
//
// compose calls On(...) repeatedly with progress ticks for the same resource
// (Working → Done). We dedupe per-resource on the rendered line so the log shows
// meaningful transitions ("redis Pulling" → "redis Pulled" → "web Started"),
// not a flood. Each new line nudges the fleet hub (coalesced latest-wins) so the
// dashboard repaints promptly during a long up.
// ---------------------------------------------------------------------------

type jobEventProcessor struct {
	job          *Job
	fleetTrigger func()

	mu   sync.Mutex
	last map[string]string // resource ID → last emitted line
}

func (p *jobEventProcessor) Start(_ context.Context, operation string) {
	if operation != "" && operation != api.ResourceCompose {
		p.job.appendLine(operation)
	}
}

func (p *jobEventProcessor) On(events ...api.Resource) {
	p.mu.Lock()
	var emitted bool
	for _, e := range events {
		line := renderResource(e)
		if line == "" || p.last[e.ID] == line {
			continue
		}
		p.last[e.ID] = line
		p.job.appendLine(line)
		emitted = true
	}
	p.mu.Unlock()
	if emitted && p.fleetTrigger != nil {
		p.fleetTrigger()
	}
}

// Done is intentionally a no-op for the success verdict. compose v5.1.4 calls
// this as `bus.Done(operation, err != nil)` (pkg/compose/progress.go) — the
// second arg is INVERTED relative to its `success bool` name (true on FAILURE).
// So never trust it: the engine derives the authoritative result from the op's
// returned error and appends the terminal "compose <op> <name> ok" / "error:"
// line itself. The update engine carries the same caveat.
func (p *jobEventProcessor) Done(string, bool) {}

// renderResource turns one compose Resource event into a compact log line:
// "<id> <text> <details>" (e.g. "redis Pulled", "web-1 Started").
func renderResource(e api.Resource) string {
	parts := make([]string, 0, 3)
	if e.ID != "" && e.ID != api.ResourceCompose {
		parts = append(parts, e.ID)
	}
	if t := strings.TrimSpace(e.Text); t != "" {
		parts = append(parts, t)
	}
	if d := strings.TrimSpace(e.Details); d != "" {
		parts = append(parts, d)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// jobLineWriter is the io.Writer compose's Out/Err streams are pointed at. It
// splits the byte stream on newlines and appends each non-empty line to the job
// log (the same chokepoint the EventProcessor uses), so any direct compose
// writes (warnings, pull text) are captured too. Concurrency-safe: compose may
// write from multiple goroutines.
type jobLineWriter struct {
	job *Job
	mu  sync.Mutex
	buf []byte
}

func (w *jobLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			w.job.appendLine(line)
		}
	}
	return len(p), nil
}
