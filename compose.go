package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
)

// composeBackend is the in-process Docker Compose v5 SDK binding. Compose ops
// (up/down/pull/restart/recreate) run as Go library calls over the same docker
// socket the fleet uses — NO `docker compose` subprocess, no stdout scraping
// (architectural decision #1). One command.Cli is built at startup and reused;
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
	// Private-registry pulls (Phase 3) read auth from the mounted config.json;
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
	return &composeBackend{cli: dockerCli, base: base}, nil
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
		// posture. Health-wait + rollback is the Phase-3.5 `update` op.
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
// by the Phase-3.5 update engine, which mutates project.Services[*].Image
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
func (b *composeBackend) loadProject(ctx context.Context, e ProjectEntry) (*types.Project, error) {
	return b.base.LoadProject(ctx, api.ProjectLoadOptions{
		ProjectName: e.Name,
		ConfigPaths: e.absComposeFiles(),
		WorkingDir:  e.WorkingDir,
		EnvFiles:    e.EnvFiles,
		Profiles:    e.Profiles,
	})
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
// line itself. (Phase 3.5: same caveat for the update engine.)
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
