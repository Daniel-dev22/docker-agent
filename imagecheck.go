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
	FilterBranch  string   `json:"filter_branch"`
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

	interval    time.Duration
	ttl         time.Duration
	concurrency int
	arch        string
	jitterSeed  string // node name → deterministic per-node jitter

	mu          sync.RWMutex
	cache       map[string]imageCheck       // compositeKey → result
	overrides   map[string]strategyOverride // override key → override
	lastFullRun time.Time

	trigger chan struct{}
}

func newImageChecker(cfg Config, dc *dockerClient, cc *http.Client) *imageChecker {
	reg := newRegistryClient(cfg.RegistryAuthFile, splitCSV(getEnv("DOCKER_REGISTRY_INSECURE", "")))
	return &imageChecker{
		docker:      dc,
		registry:    reg,
		github:      newGithubClient(cc, cfg.ControlCenterURL, reg),
		cc:          cc,
		ccURL:       cfg.ControlCenterURL,
		interval:    getEnvDuration("DOCKER_IMAGECHECK_INTERVAL", 15*time.Minute),
		ttl:         getEnvDuration("DOCKER_IMAGECHECK_TTL", 5*time.Minute),
		concurrency: imageCheckConcurrency(),
		arch:        getEnv("DOCKER_IMAGE_ARCH", "amd64"),
		jitterSeed:  cfg.NodeName,
		cache:       map[string]imageCheck{},
		overrides:   map[string]strategyOverride{},
		trigger:     make(chan struct{}, 1),
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
	ic.mu.RLock()
	since := time.Since(ic.lastFullRun)
	ic.mu.RUnlock()
	if ondemand && since < ic.ttl {
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
			res := ic.checkUnit(gctx, rep)
			rmu.Lock()
			results[k] = res
			rmu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	ic.mu.Lock()
	ic.cache = results // replace wholesale → prunes vanished images
	ic.lastFullRun = time.Now()
	ic.mu.Unlock()

	var outdated int
	for _, r := range results {
		if r.ImageStatus == statusOutdated {
			outdated++
		}
	}
	slog.Info("imagecheck pass complete", "images", len(results), "outdated", outdated, "ondemand", ondemand)
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
	movingKeyword = map[string]bool{
		"latest": true, "stable": true, "release": true, "sts": true, "alpine": true,
		"edge": true, "dev": true, "devel": true, "nightly": true, "main": true,
		"master": true, "rolling": true, "current": true,
	}
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
	return effStrategy{source: "registry-digest", origin: "auto"}
}

// checkUnit computes one image's status.
func (ic *imageChecker) checkUnit(ctx context.Context, c ContainerStatus) imageCheck {
	ref := parseImageReference(c.Image, "latest")
	res := imageCheck{Image: c.Image, ImageStatus: statusUnknown, VersionStatus: statusUnknown, CheckedAt: time.Now().Unix()}

	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	info, err := ic.docker.inspectImage(ictx, c.ImageID)
	cancel()
	if err != nil {
		res.Error = "inspect: " + err.Error()
		return res
	}
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
	q := releaseQuery{Repo: repo, ReleaseType: strat.o.ReleaseType, NameFilter: strat.o.NameFilter, TagExclude: strat.o.TagExclude}
	if q.ReleaseType == "" {
		q.ReleaseType = "latest"
	}
	rctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	tag, err := ic.github.latestRelease(rctx, q)
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
