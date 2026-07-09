package main

// Image-outdated detection (Phase 3) — the in-agent replacement for Portainer's
// /api/stacks/{id}/images_status + the server_metrics custom logic.
//
// Runs as a SLOW, JITTERED, CAPPED, CACHED pass (never inline in the fleet
// snapshot — architectural decision: the snapshot is one bounded list call, this
// is a separate hot-path-isolated pass). One check per UNIQUE (project, service,
// image) tuple; results are stamped onto containers/projects during the fleet
// merge (fleet.go).
//
// Per image the effective strategy is `central_override ?? auto_detected`:
//   - auto-detect (zero-config, drift-free — derived from the image): tag shape
//     (full X.Y.Z = pinned → github-release when an OCI source label exists, else
//     registry-digest; X.Y / X / keyword = moving → registry-digest) + the OCI
//     `source`/`version` labels.
//   - central override (docker_stack_strategies, pulled from controller) wins
//     for the un-inferable oddballs (immich multi-service, genmon build, frigate
//     github-branch).
// Repo-driven candidates pass a registry-existence gate (don't propose a tag
// whose image isn't pushed yet). GitHub auth reuses build-agent's App token vend.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// imageCheck is the per-image result. The keys image_status / latest_image_version
// / version_status are a HARD CONTRACT consumed by the Phase-4 discovery feed.
type imageCheck struct {
	Image              string `json:"image"`
	ImageStatus        string `json:"image_status"`   // updated|outdated|unknown
	VersionStatus      string `json:"version_status"` // contract alias of image_status
	CurrentVersion     string `json:"current_version,omitempty"`
	LatestImageVersion string `json:"latest_image_version,omitempty"`
	VersionSource      string `json:"version_source,omitempty"` // auto:…|override:…
	Error              string `json:"error,omitempty"`
	CheckedAt          int64  `json:"checked_at"`
}

const (
	statusUpdated  = "updated"
	statusOutdated = "outdated"
	statusUnknown  = "unknown"
)

// strategyOverride mirrors a docker_stack_strategies row (pulled from the
// controller). Match by project name (optionally one service) or image repo.
type strategyOverride struct {
	Key           string   `json:"key"`
	MatchType     string   `json:"match_type"` // project|image
	VersionSource string   `json:"version_source"`
	GithubRepo    string   `json:"github_repo"`
	ReleaseType   string   `json:"release_type"`
	NameFilter    string   `json:"name_filter"`
	TagExclude    string   `json:"tag_exclude"`
	Owner         string   `json:"owner"`
	Package       string   `json:"package"`
	FilterBranch  string   `json:"filter_branch"` // github-branch: branch to EXCLUDE (e.g. master)
	DevBranch     string   `json:"dev_branch"`    // github-branch: branch a build MUST be on (e.g. dev). Empty ⇒ exclude-only.
	ArchSuffix    string   `json:"arch_suffix"`
	TagExcludes   []string `json:"tag_excludes"`
	Image         string   `json:"image"`
	Service       string   `json:"service"`

	// Resolver half (Phase 3.5) — the `update` engine's deploy step. VersionSource
	// (above) answers "is something newer?"; these answer "what do I deploy?".
	// ImageResolver defaults are derived from VersionSource when empty (see
	// resolvers.go resolverFor): registry-digest→registry, github-release→registry
	// (single-service) / upstream-compose (immich, when ServiceMap set),
	// github-branch→registry, override→override.
	ImageResolver    string            `json:"image_resolver,omitempty"`    // registry|upstream-compose|build-agent|override
	ServiceMap       map[string]string `json:"service_map,omitempty"`       // upstream-compose: local service → upstream service name
	UpstreamCompose  string            `json:"upstream_compose,omitempty"`  // upstream compose path template ({tag} substituted); default immich's docker/docker-compose.yml
	BuildDescriptor  string            `json:"build_descriptor,omitempty"`  // build-agent: descriptor/service to build (e.g. "genmon")
	BuildBranch      string            `json:"build_branch,omitempty"`      // build-agent: branch override (frigate custom)
	RollbackScope    string            `json:"rollback_scope,omitempty"`    // per-container (default) | whole-stack (immich)
	HealthStrategy   string            `json:"health_strategy,omitempty"`   // docker-health-wait (default) | http-probe
	HealthContainers []string          `json:"health_containers,omitempty"` // subset of containers to health-check (frigate)
	HealthExcludes   []string          `json:"health_excludes,omitempty"`   // containers with no shell/healthcheck (portainer, adguardhome-sync)
}

// effStrategy is the resolved per-image strategy (auto or override).
type effStrategy struct {
	source string // registry-digest|github-release|github-branch|override
	origin string // auto|override
	o      strategyOverride
	repo   string // auto-detected source repo (github-release)
	flag   string // e.g. "pinned-no-source"
}

func (e effStrategy) label() string {
	l := e.origin + ":" + e.source
	if e.flag != "" {
		l += " (" + e.flag + ")"
	}
	return l
}

type imageChecker struct {
	docker   *dockerClient
	registry *registryClient
	github   *githubClient
	cc       *http.Client
	ccURL    string

	interval      time.Duration
	ttl           time.Duration
	forceDebounce time.Duration // min gap between admitted refresh-endpoint forces
	concurrency   int
	arch          string
	jitterSeed    string // node name → deterministic per-node jitter

	mu          sync.RWMutex
	cache       map[string]imageCheck       // compositeKey → result
	overrides   map[string]strategyOverride // override key → override
	projPlans   map[string]projPlan         // coupled/special project name → resolver plan
	lastFullRun time.Time
	forceNext   bool      // defeat the TTL coalesce for the next pass (post-update recheck)
	lastForce   time.Time // last time a debounced force was admitted (refresh endpoint)

	trigger   chan struct{}
	afterPass func() // Phase 4: invoked after each pass to refresh the discovery feed.
	// projectPlanner runs a project's resolver as a read-only dry-run (engine.planProject).
	// Set by the app; nil-safe. Lets the pass derive a coupled project's status from
	// what its update WOULD change, instead of independent per-service checks.
	projectPlanner func(ctx context.Context, name string, containers []ContainerStatus) (string, map[string]string, error)
}

// projPlan is a cached resolver dry-run for a coupled/special (non-registry)
// project: the services its update would change, the resolver kind, and any error.
type projPlan struct {
	kind    string
	targets map[string]string
	err     string
	at      time.Time // when a SUCCESSFUL plan was computed; zero when err is set
}

// mergeProjPlans keeps a project's previous SUCCESSFUL plan when this pass failed
// to compute one and the retained plan is still younger than ttl.
//
// A coupled plan depends on an upstream fetch that can fail transiently, and those
// failures are invisible: planProject passes a no-op logger, so a bad pass silently
// flipped the stack's card to "unknown" for a whole interval. Retaining the last
// good plan absorbs a blip; a persistent failure still surfaces once the retained
// plan ages out.
//
// Detection only. The update path (engine.updateProject) must keep failing loudly
// on a resolve error, and does — it never reads this map. Projects absent from
// next are dropped, so a vanished stack never lingers.
func mergeProjPlans(prev, next map[string]projPlan, now time.Time, ttl time.Duration) map[string]projPlan {
	for name, np := range next {
		if np.err == "" {
			continue
		}
		pp, ok := prev[name]
		if !ok || pp.err != "" || pp.at.IsZero() {
			continue // nothing good to fall back to
		}
		if now.Sub(pp.at) < ttl {
			next[name] = pp
		}
	}
	return next
}

// setAfterPass registers a callback fired at the end of every completed pass
// (Phase 4 wires the discovery feed here so newly-detected image status is
// pushed promptly instead of waiting for the discovery tick).
func (ic *imageChecker) setAfterPass(fn func()) { ic.afterPass = fn }

// setProjectPlanner wires engine.planProject so the pass can derive coupled
// projects' status from their resolver dry-run (DRY: detection == the update plan).
func (ic *imageChecker) setProjectPlanner(fn func(ctx context.Context, name string, containers []ContainerStatus) (string, map[string]string, error)) {
	ic.projectPlanner = fn
}

func newImageChecker(cfg Config, dc *dockerClient, cc *http.Client) *imageChecker {
	reg := newRegistryClient(cfg.RegistryAuthFile, splitCSV(getEnv("DOCKER_REGISTRY_INSECURE", "")))
	return &imageChecker{
		docker:        dc,
		registry:      reg,
		github:        newGithubClient(cc, cfg.ControlCenterURL, reg),
		cc:            cc,
		ccURL:         cfg.ControlCenterURL,
		interval:      getEnvDuration("DOCKER_IMAGECHECK_INTERVAL", 15*time.Minute),
		ttl:           getEnvDuration("DOCKER_IMAGECHECK_TTL", 5*time.Minute),
		forceDebounce: getEnvDuration("DOCKER_IMAGECHECK_FORCE_DEBOUNCE", 15*time.Second),
		concurrency:   imageCheckConcurrency(),
		arch:          getEnv("DOCKER_IMAGE_ARCH", "amd64"),
		jitterSeed:    cfg.NodeName,
		cache:         map[string]imageCheck{},
		overrides:     map[string]strategyOverride{},
		projPlans:     map[string]projPlan{},
		trigger:       make(chan struct{}, 1),
	}
}

// imageCheckConcurrency derives a low cap from the cgroup-accurate GOMAXPROCS
// (the DUPLICACY_MAX_CONCURRENT_* posture) so the digest+GitHub fan-out stays
// gentle on a Pi. Env-overridable.
func imageCheckConcurrency() int {
	n := getEnvInt("DOCKER_IMAGECHECK_CONCURRENCY", defaultBulkConcurrency())
	if n < 1 {
		n = 1
	}
	return n
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Run drives the periodic pass + on-demand triggers. A per-node jittered first
// fire avoids a fleet-wide thundering herd on the controller/GitHub (the jitter
// rule). The pass is gated by the TTL so rapid triggers coalesce.
func (ic *imageChecker) Run(ctx context.Context) {
	// Jittered initial delay (0..interval/2, deterministic per node).
	first := 10*time.Second + jitterFor(ic.jitterSeed, ic.interval/2)
	timer := time.NewTimer(first)
	defer timer.Stop()
	slog.Info("imagecheck started", "interval", ic.interval, "ttl", ic.ttl,
		"concurrency", ic.concurrency, "first_in", first)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			ic.runOnce(ctx, false)
			timer.Reset(ic.interval + jitterFor(ic.jitterSeed, 30*time.Second))
		case <-ic.trigger:
			ic.runOnce(ctx, true)
			// keep the periodic cadence; do not reset to a tiny interval
		}
	}
}

// Trigger requests an out-of-band pass (coalesced, TTL-gated).
func (ic *imageChecker) Trigger() {
	select {
	case ic.trigger <- struct{}{}:
	default:
	}
}

// ForceRecheck requests an out-of-band pass that BYPASSES the TTL coalesce. Called
// right after a successful update so the new image status reaches the fleet snapshot
// and the discovery feed within seconds — otherwise both keep the pre-update status
// for up to one interval, and the controller's smart-routing re-targets an
// already-updated stack (sending an update where it should have skipped).
func (ic *imageChecker) ForceRecheck() {
	ic.mu.Lock()
	ic.forceNext = true
	ic.mu.Unlock()
	ic.Trigger()
}

// ForceRecheckDebounced is the user-/test-facing refresh: it forces a non-coalesced
// pass (so a just-changed strategy is reloaded and recomputed immediately, not up to
// one TTL later) but admits at most one force per forceDebounce window. A spammed
// refresh button thus collapses to a single forced pass plus cheap coalesced triggers,
// instead of running back-to-back full snapshot+digest fan-outs.
func (ic *imageChecker) ForceRecheckDebounced() {
	ic.mu.Lock()
	if time.Since(ic.lastForce) >= ic.forceDebounce {
		ic.forceNext = true
		ic.lastForce = time.Now()
	}
	ic.mu.Unlock()
	ic.Trigger()
}

// jitterFor returns a deterministic [0,max) offset from a seed (node name) so
// each host fires at a stable but distinct phase.
func jitterFor(seed string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	var h uint32 = 2166136261
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= 16777619
	}
	return time.Duration(uint64(h) % uint64(max))
}

// runOnce performs a full pass: refresh overrides, dedupe current images by
// (project,service,image), recompute stale entries with bounded concurrency, and
// prune cache entries for images no longer present.
func (ic *imageChecker) runOnce(ctx context.Context, ondemand bool) {
	ic.mu.Lock()
	since := time.Since(ic.lastFullRun)
	force := ic.forceNext
	ic.forceNext = false
	ic.mu.Unlock()
	if ondemand && !force && since < ic.ttl {
		return // coalesce rapid triggers
	}

	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	containers, _, err := ic.docker.snapshot(sctx)
	cancel()
	if err != nil {
		slog.Warn("imagecheck snapshot failed", "error", err)
		return
	}
	ic.refreshOverrides(ctx)

	units := map[string]ContainerStatus{}
	for _, c := range containers {
		if c.Image == "" || c.ImageID == "" {
			continue
		}
		k := compositeKey(c)
		if _, ok := units[k]; !ok {
			units[k] = c
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(ic.concurrency)
	results := make(map[string]imageCheck, len(units))
	var rmu sync.Mutex
	for k, rep := range units {
		k, rep := k, rep
		g.Go(func() error {
			// Detection inspects the exact running image by ImageID (its labels +
			// RepoDigests); a failed inspect is surfaced as unknown, same as before.
			ictx, cancel := context.WithTimeout(gctx, 15*time.Second)
			info, ierr := ic.docker.inspectImage(ictx, rep.ImageID)
			cancel()
			res := imageCheck{Image: rep.Image, ImageStatus: statusUnknown, VersionStatus: statusUnknown, CheckedAt: time.Now().Unix()}
			if ierr != nil {
				res.Error = "inspect: " + ierr.Error()
			} else {
				res = ic.checkUnit(gctx, rep, info)
			}
			rmu.Lock()
			results[k] = res
			rmu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	// Project-level dry-run plans for coupled/special (non-registry) projects: a
	// project is outdated iff its update WOULD change ≥1 service — so a coupled
	// service with its own newer upstream can't make the stack look outdated.
	// Registry projects stay per-service (the rollup above). Computed in the pass
	// (it can do GitHub/registry calls); stampImageStatus only reads the cache.
	now := time.Now()
	plans := map[string]projPlan{}
	if ic.projectPlanner != nil {
		seen := map[string]bool{}
		for _, c := range containers {
			name := c.ComposeProject
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if _, ok := ic.projectOverride(name); !ok {
				continue // no override → registry path (per-service)
			}
			kind, targets, perr := ic.projectPlanner(ctx, name, containers)
			if kind == "" || kind == "registry" {
				continue
			}
			pp := projPlan{kind: kind, targets: targets}
			if perr != nil {
				pp.err = perr.Error()
			} else {
				pp.at = now
			}
			plans[name] = pp
		}
	}

	ic.mu.Lock()
	ic.cache = results // replace wholesale → prunes vanished images
	ic.projPlans = mergeProjPlans(ic.projPlans, plans, now, 2*ic.interval)
	ic.lastFullRun = time.Now()
	ic.mu.Unlock()

	var outdated int
	for _, r := range results {
		if r.ImageStatus == statusOutdated {
			outdated++
		}
	}
	slog.Info("imagecheck pass complete", "images", len(results), "outdated", outdated, "ondemand", ondemand)

	// Phase 4: nudge the discovery feed so freshly-detected image status reaches
	// the action dropdowns without waiting for the next discovery tick.
	if ic.afterPass != nil {
		ic.afterPass()
	}
}

// lookup returns the cached check for a container (by composite key).
func (ic *imageChecker) lookup(c ContainerStatus) (imageCheck, bool) {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	r, ok := ic.cache[compositeKey(c)]
	return r, ok
}

// snapshotCache returns a copy of all cached results (debug endpoint).
func (ic *imageChecker) snapshotCache() []imageCheck {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	out := make([]imageCheck, 0, len(ic.cache))
	for _, r := range ic.cache {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out
}

func compositeKey(c ContainerStatus) string {
	return c.ComposeProject + "\x00" + c.ComposeService + "\x00" + c.Image
}

// stampImageStatus copies cached image-check results onto the snapshot's
// containers and rolls them up to compose projects (outdated if ANY service is
// outdated, else updated if ALL known, else unknown). Pure in-memory map lookups
// — safe to call on the fleet hot path. Containers with no cached result yet are
// left blank (the UI shows nothing until the first pass completes).
func stampImageStatus(containers []ContainerStatus, projects []ComposeProject, ic *imageChecker) {
	byProject := map[string]*ComposeProject{}
	for i := range projects {
		byProject[projects[i].Name] = &projects[i]
	}
	type roll struct{ outdated, checked int }
	rollups := map[string]*roll{}

	for i := range containers {
		c := &containers[i]
		r, ok := ic.lookup(*c)
		if !ok {
			continue
		}
		c.ImageStatus = r.ImageStatus
		c.LatestImageVersion = r.LatestImageVersion
		c.VersionSource = r.VersionSource
		c.CurrentVersion = r.CurrentVersion
		if c.ComposeProject == "" {
			continue
		}
		rl := rollups[c.ComposeProject]
		if rl == nil {
			rl = &roll{}
			rollups[c.ComposeProject] = rl
		}
		if r.ImageStatus != "" && r.ImageStatus != statusUnknown {
			rl.checked++
		}
		if r.ImageStatus == statusOutdated {
			rl.outdated++
		}
	}

	for name, rl := range rollups {
		p := byProject[name]
		if p == nil {
			continue
		}
		p.OutdatedCount = rl.outdated
		switch {
		case rl.outdated > 0:
			p.ImageStatus = statusOutdated
		case rl.checked > 0:
			p.ImageStatus = statusUpdated
		default:
			p.ImageStatus = statusUnknown
		}
	}

	// Coupled/special projects: override the per-service rollup with the resolver
	// plan. The project is outdated iff its update would change ≥1 service, and
	// member containers' pills follow the plan — so a coupled service whose own
	// upstream moved (but the driver release did NOT) reports updated, and the
	// whole stack reports updated. This is the DRY guarantee: detection == the
	// update's dry-run.
	ic.mu.RLock()
	plans := ic.projPlans
	ic.mu.RUnlock()
	for name, pp := range plans {
		if p := byProject[name]; p != nil {
			p.OutdatedCount = len(pp.targets)
			switch {
			case pp.err != "":
				p.ImageStatus = statusUnknown
			case len(pp.targets) > 0:
				p.ImageStatus = statusOutdated
			default:
				p.ImageStatus = statusUpdated
			}
		}
		for i := range containers {
			c := &containers[i]
			if c.ComposeProject != name {
				continue
			}
			switch {
			case pp.err != "":
				c.ImageStatus = statusUnknown
			case planChanges(pp.targets, c.ComposeService):
				c.ImageStatus = statusOutdated
			default:
				c.ImageStatus = statusUpdated
			}
		}
	}
}

// planChanges reports whether a resolver plan would change the given service.
func planChanges(targets map[string]string, service string) bool {
	_, ok := targets[service]
	return ok
}

// refreshOverrides pulls the central docker_stack_strategies table from the
// controller (best-effort: keep the previous set on failure).
func (ic *imageChecker) refreshOverrides(ctx context.Context) {
	if ic.cc == nil || ic.ccURL == "" {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, ic.ccURL+"/api/docker/strategies", nil)
	if err != nil {
		return
	}
	resp, err := ic.cc.Do(req)
	if err != nil {
		slog.Debug("strategy override fetch failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var body struct {
		Strategies []strategyOverride `json:"strategies"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return
	}
	m := make(map[string]strategyOverride, len(body.Strategies))
	for _, o := range body.Strategies {
		if o.Key != "" {
			m[o.Key] = o
		}
	}
	ic.mu.Lock()
	ic.overrides = m
	ic.mu.Unlock()
}

// projectOverride returns the project-scoped override for a project name, if any
// (the resolver-selection input for the Phase-3.5 update engine). Service-scoped
// rows (o.Service != "") are intentionally NOT returned here — the engine selects
// ONE resolver per project; per-service tweaks ride the registry resolver via
// resolveStrategy/matchOverride.
func (ic *imageChecker) projectOverride(name string) (strategyOverride, bool) {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	for _, o := range ic.overrides {
		if o.MatchType == "image" {
			continue
		}
		if o.Key == name && o.Service == "" {
			return o, true
		}
	}
	return strategyOverride{}, false
}

// matchOverride finds an override for a container: project-scoped (optionally
// service-scoped) or image-scoped.
func (ic *imageChecker) matchOverride(c ContainerStatus, ref imageRef) (strategyOverride, bool) {
	ic.mu.RLock()
	defer ic.mu.RUnlock()
	for _, o := range ic.overrides {
		switch o.MatchType {
		case "image":
			if o.Key == ref.FullRepo || o.Key == c.Image {
				return o, true
			}
		default: // project
			if o.Key != c.ComposeProject {
				continue
			}
			if o.Service != "" && o.Service != c.ComposeService {
				continue
			}
			return o, true
		}
	}
	return strategyOverride{}, false
}

var (
	pinnedRe      = regexp.MustCompile(`^v?\d+\.\d+\.\d+`)
	xyTruncateRe  = regexp.MustCompile(`^v?(\d+\.\d+)\.\d+`)
	commitShaRe   = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	movingKeyword = map[string]bool{
		"latest": true, "stable": true, "release": true, "sts": true, "alpine": true,
		"edge": true, "dev": true, "devel": true, "nightly": true, "main": true,
		"master": true, "rolling": true, "current": true,
	}
	// knownArchSuffixes are stripped before classifying a tag, independent of the
	// configured DOCKER_IMAGE_ARCH (images are sometimes tagged for a non-host arch).
	knownArchSuffixes = []string{"amd64", "arm64", "aarch64", "armv7", "arm"}
)

// isPinnedTag reports whether a tag is a full pinned X.Y.Z (so the upstream
// release source is authoritative); X.Y / X / keyword tags are "moving".
func isPinnedTag(tag string) bool {
	t := strings.ToLower(tag)
	if t == "" || movingKeyword[t] {
		return false
	}
	return pinnedRe.MatchString(tag)
}

// looksLikeCommitSHATag reports whether a (non-pinned) tag is a bare git
// commit-sha, after stripping a trailing -<arch> suffix (frigate's
// "8203e39-amd64" → "8203e39"). Such a tag is immutable, so same-tag
// registry-digest can never see a newer build — it must be resolved by listing
// the ghcr package's tags instead. Only the unambiguous bare-sha shape is
// auto-detected; X.Y-<sha> dev tags collide with moving variant tags
// (e.g. "1.25-alpine") and stay override-only.
func looksLikeCommitSHATag(tag, arch string) bool {
	t := tag
	if arch != "" {
		t = strings.TrimSuffix(t, "-"+arch)
	}
	for _, a := range knownArchSuffixes {
		t = strings.TrimSuffix(t, "-"+a)
	}
	return commitShaRe.MatchString(t)
}

// splitOwnerPackage splits a ghcr repository path "owner/pkg[/sub]" into the
// owner and the package name (the GitHub Packages API container name).
func splitOwnerPackage(repository string) (owner, pkg string) {
	parts := strings.SplitN(repository, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return repository, ""
}

// sourceRepoFromLabels reads org.opencontainers.image.source (a github URL) and
// falls back to a ghcr image path. Returns owner/repo or "".
func sourceRepoFromLabels(labels map[string]string, ref imageRef) string {
	if s := labels["org.opencontainers.image.source"]; s != "" {
		s = strings.TrimSuffix(strings.TrimSpace(s), "/")
		s = strings.TrimPrefix(s, "https://github.com/")
		s = strings.TrimPrefix(s, "http://github.com/")
		s = strings.TrimPrefix(s, "github.com/")
		if strings.Count(s, "/") == 1 && s != "" {
			return s
		}
	}
	if ref.Registry == "ghcr.io" {
		parts := strings.SplitN(ref.Repository, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	return ""
}

// resolveStrategy returns the effective strategy: a central override wins,
// otherwise auto-detect from the image's tag shape + OCI labels.
func (ic *imageChecker) resolveStrategy(c ContainerStatus, info imageInfo, ref imageRef) effStrategy {
	if o, ok := ic.matchOverride(c, ref); ok {
		src := o.VersionSource
		if src == "" {
			src = "registry-digest"
		}
		return effStrategy{source: src, origin: "override", o: o, repo: o.GithubRepo}
	}
	repo := sourceRepoFromLabels(info.Labels, ref)
	if isPinnedTag(ref.Tag) {
		if repo != "" {
			return effStrategy{source: "github-release", origin: "auto", repo: repo}
		}
		return effStrategy{source: "registry-digest", origin: "auto", flag: "pinned-no-source"}
	}
	// Non-pinned tag. A bare commit-sha on a ghcr image is immutable, so same-tag
	// registry-digest is useless ("updated" forever) — list the ghcr package's
	// tags instead (frigate's "8203e39-amd64"). Auto-derive owner/package from the
	// ghcr path (the GitHub Packages API key) so no override row is needed.
	if ref.Registry == "ghcr.io" && repo != "" && looksLikeCommitSHATag(ref.Tag, ic.arch) {
		owner, pkg := splitOwnerPackage(ref.Repository)
		return effStrategy{
			source: "github-branch", origin: "auto", repo: repo,
			o: strategyOverride{Owner: owner, Package: pkg, GithubRepo: repo, ArchSuffix: ic.arch},
		}
	}
	// Moving tag (latest/stable/edge/X/X.Y/date) or any other unresolvable shape:
	// the tag rolls forward in place, so digest drift on the same tag detects it.
	return effStrategy{source: "registry-digest", origin: "auto"}
}

// checkUnit computes one image's status from a (best-effort) local image inspect.
// The caller owns the inspect — detection by the running container's ImageID, the
// update resolver by the compose image reference — so checkUnit classifies off the
// image *reference* plus remote GitHub/registry lookups and treats an empty `info`
// as non-fatal (current version falls back to the tag; only registry-digest needs
// the local RepoDigest). This mirrors the ansible frigate model (reference-driven,
// no hard ImageID dependency).
func (ic *imageChecker) checkUnit(ctx context.Context, c ContainerStatus, info imageInfo) imageCheck {
	ref := parseImageReference(c.Image, "latest")
	res := imageCheck{Image: c.Image, ImageStatus: statusUnknown, VersionStatus: statusUnknown, CheckedAt: time.Now().Unix()}

	current := info.Labels["org.opencontainers.image.version"]
	if current == "" {
		current = ref.Tag
	}
	res.CurrentVersion = current

	strat := ic.resolveStrategy(c, info, ref)
	res.VersionSource = strat.label()

	switch strat.source {
	case "github-release":
		ic.fillGithubRelease(ctx, &res, ref, current, strat)
	case "github-branch":
		ic.fillGithubBranch(ctx, &res, c, strat)
	case "override":
		ic.fillOverridePinned(&res, ref, strat)
	default: // registry-digest
		ic.fillRegistryDigest(ctx, &res, info, ref)
	}
	return res
}

// fillRegistryDigest compares the local content digest to the remote manifest
// digest for the same tag (moving-tag images).
func (ic *imageChecker) fillRegistryDigest(ctx context.Context, res *imageCheck, info imageInfo, ref imageRef) {
	local := localDigest(info, ref)
	if local == "" {
		res.Error = "no local RepoDigest (locally-built image?)"
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	remote, err := ic.registry.getRegistryDigest(dctx, ref)
	cancel()
	if err != nil {
		res.Error = "registry: " + err.Error()
		return
	}
	res.LatestImageVersion = ref.String()
	if strings.EqualFold(local, remote) {
		res.ImageStatus, res.VersionStatus = statusUpdated, statusUpdated
	} else {
		res.ImageStatus, res.VersionStatus = statusOutdated, statusOutdated
	}
}

// fillGithubRelease finds the newest release tag, gates it through the registry
// (the target image must actually exist), and compares versions by semver.
func (ic *imageChecker) fillGithubRelease(ctx context.Context, res *imageCheck, ref imageRef, current string, strat effStrategy) {
	repo := strat.repo
	if repo == "" {
		repo = strat.o.GithubRepo
	}
	if repo == "" {
		res.Error = "github-release: no source repo"
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	tag, err := ic.github.latestRelease(rctx, releaseQueryFor(repo, strat.o))
	cancel()
	if err != nil {
		res.Error = "github-release: " + err.Error()
		return
	}

	// Existence gate: the running image's repo must carry the release tag (try
	// v-prefix / X.Y truncation variants). Report the newest variant that exists.
	verifiedTag, verifiedImage := ic.gateReleaseImage(ctx, ref.FullRepo, tag)
	if verifiedImage == "" {
		res.ImageStatus, res.VersionStatus = statusUnknown, statusUnknown
		res.Error = fmt.Sprintf("release %s published, image not yet available in %s", tag, ref.FullRepo)
		res.LatestImageVersion = ref.FullRepo + ":" + tag
		return
	}
	res.LatestImageVersion = verifiedImage
	if versionOutdated(current, verifiedTag) {
		res.ImageStatus, res.VersionStatus = statusOutdated, statusOutdated
	} else {
		res.ImageStatus, res.VersionStatus = statusUpdated, statusUpdated
	}
}

// gateReleaseImage tries tag variants against the registry, returning the first
// that exists (most specific first) as (tag, full image ref).
func (ic *imageChecker) gateReleaseImage(ctx context.Context, repo, tag string) (string, string) {
	for _, v := range tagVariants(tag) {
		ref := parseImageReference(repo+":"+v, "latest")
		vctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		ok := ic.registry.imageExists(vctx, ref)
		cancel()
		if ok {
			return v, repo + ":" + v
		}
	}
	return "", ""
}

func tagVariants(tag string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(tag)
	if strings.HasPrefix(tag, "v") {
		add(tag[1:])
	} else {
		add("v" + tag)
	}
	if m := xyTruncateRe.FindStringSubmatch(tag); m != nil {
		add(m[1])
	}
	return out
}

func (ic *imageChecker) fillGithubBranch(ctx context.Context, res *imageCheck, c ContainerStatus, strat effStrategy) {
	o := strat.o
	ref := parseImageReference(c.Image, "latest")
	q := branchQuery{
		Owner:        o.Owner,
		Package:      o.Package,
		Repo:         o.GithubRepo,
		FilterBranch: o.FilterBranch,
		DevBranch:    o.DevBranch,
		ArchSuffix:   firstNonEmpty(o.ArchSuffix, ic.arch),
		TagExcludes:  o.TagExcludes,
		CurrentTag:   ref.Tag,
	}
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	br, err := ic.github.latestPackageVersion(bctx, q)
	cancel()
	if err != nil {
		res.Error = "github-branch: " + err.Error()
		return
	}
	res.ImageStatus, res.VersionStatus = br.VersionStatus, br.VersionStatus
	res.LatestImageVersion = br.LatestImage
}

// fillOverridePinned handles an explicit pinned-image override: outdated iff the
// running image differs from the pinned target.
func (ic *imageChecker) fillOverridePinned(res *imageCheck, ref imageRef, strat effStrategy) {
	target := strat.o.Image
	if target == "" {
		res.Error = "override: no image"
		return
	}
	res.LatestImageVersion = target
	if ref.String() == target {
		res.ImageStatus, res.VersionStatus = statusUpdated, statusUpdated
	} else {
		res.ImageStatus, res.VersionStatus = statusOutdated, statusOutdated
	}
}

// localDigest extracts the sha256 from the RepoDigest matching ref's repo.
func localDigest(info imageInfo, ref imageRef) string {
	want := ref.FullRepo
	for _, rd := range info.RepoDigests {
		at := strings.Index(rd, "@")
		if at < 0 {
			continue
		}
		repo, dig := rd[:at], rd[at+1:]
		if repo == want || strings.HasSuffix(repo, "/"+want) || strings.HasSuffix(want, "/"+strings.TrimPrefix(repo, "library/")) {
			return dig
		}
	}
	if len(info.RepoDigests) > 0 {
		if at := strings.Index(info.RepoDigests[0], "@"); at >= 0 {
			return info.RepoDigests[0][at+1:]
		}
	}
	return ""
}

// versionOutdated reports whether latest is a newer version than current. Falls
// back to normalized string inequality when either can't be parsed as semver.
func versionOutdated(current, latest string) bool {
	cv, lv := parseSemver(current), parseSemver(latest)
	if cv == (semver{}) || lv == (semver{}) {
		return normVer(current) != normVer(latest) && latest != ""
	}
	return lv.cmp(cv) > 0
}

func normVer(s string) string { return strings.TrimLeft(strings.TrimSpace(s), "vV") }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
