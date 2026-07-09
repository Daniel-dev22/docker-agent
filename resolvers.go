package main

// Per-stack image resolvers (Phase 3.5) — the only pluggable step in the shared
// update engine (stackengine.go). VersionSource (Phase 3, imagecheck.go) answers
// "is something newer?"; an ImageResolver answers "what do I deploy?".
//
// Strategy selection mirrors imagecheck's `central_override ?? auto-detect`:
//   - The DEFAULT path (registryResolver) reuses imagecheck.checkUnit per service,
//     so auto-detect, github-release/branch sources, the registry-existence gate,
//     and image-scoped overrides all behave EXACTLY as the status check — no
//     second implementation to drift.
//   - A PROJECT-scoped central override whose image_resolver can't be expressed
//     per-service selects a dedicated resolver: upstream-compose (immich's coupled
//     multi-service tags), build-agent (genmon/frigate-custom — build stays in
//     build-agent), or override (an explicit pinned image for the whole stack).
//   - A request-level override_image (the traefik/manual "deploy this exact tag"
//     path) wins over everything for the targeted service.
//
// A resolver returns ONLY the services whose image should change. Services absent
// from the map keep their compose image and are still `pull`ed — so a moving-tag
// (registry-digest) refresh needs no rewrite at all; the new digest is pulled and
// `up` recreates.

import (
	"context"
	"fmt"
	"log/slog"
	neturl "net/url"
	"regexp"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// ImageResolver decides the target image for each service to change in a project.
//
// plan is a read-only dry-run: it returns the service→image change set WITHOUT
// applying anything. The update engine (stackengine.go) runs it then applies;
// the image checker (imagecheck.go) runs it to derive a project's outdated
// status — so "is it outdated?" and "what do I deploy?" are the SAME answer and
// can never diverge (e.g. a coupled stack is outdated iff its driver release
// would actually change an image, never because a coupled service has its own
// newer upstream). `log` receives progress lines (nil-safe via a no-op caller).
type ImageResolver interface {
	plan(ctx context.Context, project *types.Project, log func(string)) (map[string]string, error)
	kind() string
}

// resolveMeta carries the project-level knobs the engine needs beyond the image
// map: rollback scope, health strategy, and the health-check subset/excludes.
// Populated from the project-scoped override (defaults otherwise).
type resolveMeta struct {
	rollbackScope    string   // per-container (default) | whole-stack
	healthStrategy   string   // docker-health-wait (default) | http-probe
	healthContainers []string // only-check subset (frigate)
	healthExcludes   []string // no-shell containers (portainer, adguardhome-sync)
}

// defaultHealthExcludes mirror manage_portainer_stack_update.yaml's
// excluded_container_health_statuses (no builtin shell → no healthcheck).
var defaultHealthExcludes = []string{"portainer", "adguardhome-sync"}

// selectResolver picks the resolver + project-level meta for an update. The
// imageChecker (e.images) supplies overrides + the shared registry/github clients;
// when nil, only the registry/manual paths work (no version resolution).
func (e *engine) selectResolver(projectName, overrideImage, overrideService string) (ImageResolver, resolveMeta) {
	meta := resolveMeta{
		rollbackScope:  "per-container",
		healthStrategy: "docker-health-wait",
		healthExcludes: defaultHealthExcludes,
	}

	// Request-level manual override wins (traefik "deploy this exact image").
	if strings.TrimSpace(overrideImage) != "" {
		return &overrideResolver{e: e, image: overrideImage, service: overrideService}, meta
	}

	var o strategyOverride
	var ok bool
	if e.images != nil {
		o, ok = e.images.projectOverride(projectName)
	}
	if ok {
		if o.RollbackScope != "" {
			meta.rollbackScope = o.RollbackScope
		}
		if o.HealthStrategy != "" {
			meta.healthStrategy = o.HealthStrategy
		}
		if len(o.HealthContainers) > 0 {
			meta.healthContainers = o.HealthContainers
		}
		if len(o.HealthExcludes) > 0 {
			meta.healthExcludes = o.HealthExcludes
		}
		switch resolverKind(o) {
		case "upstream-compose":
			return &upstreamComposeResolver{e: e, o: o}, meta
		case "build-agent":
			return &buildAgentResolver{e: e, o: o}, meta
		case "override":
			return &overrideResolver{e: e, image: o.Image, service: o.Service}, meta
		}
	}
	return &registryResolver{e: e}, meta
}

// planProject runs the project's resolver as a read-only dry-run to compute its
// image change set — the single source of truth for both detection (is the
// project outdated?) and the update engine. The synthetic project is built from
// the RUNNING containers (service → running image), so the plan reflects what
// the stack is actually running. Registry-kind projects are detected per-service
// by the image checker (checkUnit), so planProject skips them (returns "registry"
// + nil) and the per-service rollup stands.
func (e *engine) planProject(ctx context.Context, name string, containers []ContainerStatus) (kind string, targets map[string]string, err error) {
	resolver, _ := e.selectResolver(name, "", "")
	kind = resolver.kind()
	if kind == "registry" {
		return kind, nil, nil
	}
	svcs := types.Services{}
	for _, c := range containers {
		if c.ComposeProject != name || c.ComposeService == "" || c.Image == "" {
			continue
		}
		if _, ok := svcs[c.ComposeService]; ok {
			continue
		}
		svcs[c.ComposeService] = types.ServiceConfig{Name: c.ComposeService, Image: c.Image}
	}
	if len(svcs) == 0 {
		return kind, nil, nil // project not running → nothing to plan
	}
	project := &types.Project{Name: name, Services: svcs}
	targets, err = resolver.plan(ctx, project, func(string) {})
	return kind, targets, err
}

// tracksMovingTag reports whether an image reference uses a moving tag
// (release/latest/edge/…). Such a service auto-follows upstream, so a coupled
// (upstream-compose) plan must not flag or rewrite it to the release's concrete
// pin — only concretely-pinned coupled services drive the stack's status.
func tracksMovingTag(image string) bool {
	return movingKeyword[strings.ToLower(parseImageReference(image, "latest").Tag)]
}

// resolverKind returns the effective image_resolver: the explicit column if set,
// else derived from version_source (immich's github-release + a service_map ⇒
// upstream-compose; everything else ⇒ registry/override).
func resolverKind(o strategyOverride) string {
	if o.ImageResolver != "" {
		return o.ImageResolver
	}
	if len(o.ServiceMap) > 0 || o.UpstreamCompose != "" {
		return "upstream-compose"
	}
	if o.VersionSource == "override" || o.Image != "" {
		return "override"
	}
	return "registry"
}

// ---------------------------------------------------------------------------
// registryResolver — the default. Per service, reuse imagecheck.checkUnit so the
// deploy target is exactly the status check's verified `latest_image_version`.
// ---------------------------------------------------------------------------

type registryResolver struct{ e *engine }

func (r *registryResolver) kind() string { return "registry" }

func (r *registryResolver) plan(ctx context.Context, project *types.Project, log func(string)) (map[string]string, error) {
	ic := r.e.images
	if ic == nil {
		log("no image checker — pull-only update (no version resolution)")
		return map[string]string{}, nil
	}
	ic.refreshOverrides(ctx) // freshest central overrides for this update
	targets := map[string]string{}
	for name, svc := range project.Services {
		if svc.Image == "" {
			continue // build-only service: nothing to resolve/pull by tag
		}
		ref := parseImageReference(svc.Image, "latest")
		// Best-effort inspect by the compose image *reference* (may be unpulled); the
		// reference is what we deploy, and checkUnit tolerates an empty info.
		info, _ := r.e.docker.inspectImage(ctx, svc.Image)
		synthC := ContainerStatus{ComposeProject: project.Name, ComposeService: name, Image: svc.Image}
		strat := ic.resolveStrategy(synthC, info, ref)
		chk := ic.checkUnit(ctx, synthC, info)

		// registry-digest = moving tag: pull always refreshes the digest, so an empty
		// target (incl. an "unknown" from an unpulled image) is correct — no rewrite.
		if strat.source == "registry-digest" {
			if chk.ImageStatus == statusOutdated {
				log(fmt.Sprintf("%s: newer digest for %s (pull)", name, svc.Image))
			}
			continue
		}

		// Version-resolved (github-branch / github-release / override): we MUST resolve a
		// concrete target. A failure here is LOUD — never silently redeploy the old pin
		// and report success.
		target, perr := planServiceTarget(name, svc.Image, chk)
		if perr != nil {
			return nil, perr
		}
		if target != "" {
			targets[name] = target
			log(fmt.Sprintf("%s: %s → %s (%s)", name, svc.Image, target, chk.VersionSource))
		}
	}
	return targets, nil
}

// planServiceTarget decides the update target for one VERSION-RESOLVED service
// (registry-digest is handled by the caller). It returns:
//   - (tag, nil)  → rewrite the service image to tag,
//   - ("",  nil)  → nothing to do (already current),
//   - ("",  err)  → LOUD failure: the service is outdated/uncheckable but we could not
//     resolve a concrete target, so the update must fail rather than silently redeploy
//     the existing pin and report success.
func planServiceTarget(name, svcImage string, chk imageCheck) (string, error) {
	switch chk.ImageStatus {
	case statusOutdated:
		if chk.LatestImageVersion == "" {
			return "", fmt.Errorf("%s: %s is outdated but the resolver produced no target image (%s)", name, svcImage, chk.VersionSource)
		}
		if chk.LatestImageVersion != svcImage {
			return chk.LatestImageVersion, nil
		}
		return "", nil // resolved target equals current image — nothing to do
	case statusUnknown:
		return "", fmt.Errorf("%s: version check failed for %s (%s) — refusing to update from incomplete data", name, svcImage, chk.Error)
	}
	return "", nil // statusUpdated → already current
}

// ---------------------------------------------------------------------------
// overrideResolver — deploy an explicit image (traefik / manual override). The
// target service is the request's hint, else the service named like the project,
// else the sole service.
// ---------------------------------------------------------------------------

type overrideResolver struct {
	e       *engine
	image   string
	service string
}

func (r *overrideResolver) kind() string { return "override" }

func (r *overrideResolver) plan(_ context.Context, project *types.Project, log func(string)) (map[string]string, error) {
	if strings.TrimSpace(r.image) == "" {
		return nil, fmt.Errorf("override resolver: no image given")
	}
	svc := r.service
	if svc == "" {
		if _, ok := project.Services[project.Name]; ok {
			svc = project.Name
		} else if len(project.Services) == 1 {
			for name := range project.Services {
				svc = name
			}
		}
	}
	if svc == "" {
		return nil, fmt.Errorf("override resolver: cannot infer target service for %q (specify service)", project.Name)
	}
	cur, ok := project.Services[svc]
	if !ok {
		return nil, fmt.Errorf("override resolver: service %q not in project %q", svc, project.Name)
	}
	// Already on the pinned image → empty plan (so detection reports updated and
	// the engine skips a needless force-recreate).
	if cur.Image == r.image {
		return map[string]string{}, nil
	}
	log(fmt.Sprintf("override: %s → %s", svc, r.image))
	return map[string]string{svc: r.image}, nil
}

// ---------------------------------------------------------------------------
// upstreamComposeResolver — immich. Read the upstream release's docker-compose.yml
// and map its coupled service tags onto our local services (we PULL, never build).
// Ports get_immich_stack_image_versions.yaml.
// ---------------------------------------------------------------------------

type upstreamComposeResolver struct {
	e *engine
	o strategyOverride
}

func (r *upstreamComposeResolver) kind() string { return "upstream-compose" }

func (r *upstreamComposeResolver) plan(ctx context.Context, project *types.Project, log func(string)) (map[string]string, error) {
	ic := r.e.images
	if ic == nil || ic.github == nil {
		return nil, fmt.Errorf("upstream-compose resolver: image checker unavailable")
	}
	repo := r.o.GithubRepo
	if repo == "" {
		return nil, fmt.Errorf("upstream-compose resolver: github_repo required")
	}
	rctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	tag, err := ic.github.latestRelease(rctx, releaseQueryFor(repo, r.o))
	cancel()
	if err != nil {
		return nil, fmt.Errorf("upstream-compose: latest release: %w", err)
	}
	tmpl := r.o.UpstreamCompose
	if tmpl == "" {
		tmpl = "https://" + rawGithubHost + "/" + repo + "/{tag}/docker/docker-compose.yml"
	}
	url := strings.ReplaceAll(tmpl, "{tag}", tag)
	log(fmt.Sprintf("immich: latest release %s — reading %s", tag, url))

	// immich's compose templates the app images as ${IMMICH_VERSION:-release}; pin
	// them to the concrete release tag (the whole point of the coupled update) so
	// the rewrite is a real version, not the floating `release` tag.
	//
	// The budget is wider than one round trip because composeImages retries
	// transient 429/5xx with backoff before giving up.
	fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	upstream, err := ic.github.composeImages(fctx, url, map[string]string{"IMMICH_VERSION": tag})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("upstream-compose: %w", err)
	}

	// service_map maps local service name → upstream service name. Identity when
	// the override omits it.
	m := r.o.ServiceMap
	targets := map[string]string{}
	for name, svc := range project.Services {
		upName := name
		if m != nil {
			if mapped, ok := m[name]; ok {
				upName = mapped
			}
		}
		img, ok := upstream[upName]
		if !ok || img == "" {
			continue
		}
		// A service intentionally on a MOVING tag (immich-server ":release") is
		// auto-tracking latest, so it is never "behind a release" — don't flag or
		// rewrite it to the release's concrete pin (a plain `pull` refreshes it).
		// Only concretely-pinned coupled services (redis/db @sha256, or a pinned
		// X.Y.Z) are driven by the release compose. This keeps the WHOLE-STACK
		// status driven by the actual drivers, never by a coupled service's own
		// independent upstream.
		if tracksMovingTag(svc.Image) {
			continue
		}
		if img != svc.Image {
			targets[name] = img
			log(fmt.Sprintf("%s: %s → %s (upstream %s)", name, svc.Image, img, upName))
		}
	}
	if len(targets) == 0 {
		log("immich: already on the latest coupled image set")
	}
	return targets, nil
}

// composeImagesDoc is the minimal upstream compose shape we read.
type composeImagesDoc struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

// composeVarRe matches a compose interpolation `${VAR}`, `${VAR:-default}`, or
// `${VAR-default}` (the forms immich's compose uses for image tags).
var composeVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}`)

// interpolateComposeVars resolves `${VAR...}` against vars, falling back to the
// `:-default` (or empty) when the var is absent — matching docker compose's own
// substitution for the variables we know (e.g. IMMICH_VERSION).
func interpolateComposeVars(s string, vars map[string]string) string {
	return composeVarRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := composeVarRe.FindStringSubmatch(m)
		if v, ok := vars[sub[1]]; ok && v != "" {
			return v
		}
		return sub[2] // default group (empty when the form was bare `${VAR}`)
	})
}

// rawGithubHost serves public repo files anonymously, with a bursty per-IP rate
// limit. Everything it serves is also reachable through the authenticated
// Contents API, which is what we actually use.
const rawGithubHost = "raw.githubusercontent.com"

// composeCacheMax bounds composeCache. Entries are a handful of services and grow
// by one key per release tag observed, so this is a runaway backstop rather than a
// working-set limit — clearing merely costs one re-fetch.
const composeCacheMax = 64

// parseRawGithubURL splits
// https://raw.githubusercontent.com/{owner}/{name}/{ref}/{path...}
// into its Contents-API components. ok=false for any other host, so an
// upstream_compose pointing at a non-GitHub server still works over a plain GET.
//
// This is the single switch point for how a compose body is transported.
func parseRawGithubURL(raw string) (repo, ref, path string, ok bool) {
	u, err := neturl.Parse(raw)
	if err != nil || u.Host != rawGithubHost {
		return "", "", "", false
	}
	// A query or fragment carries intent the Contents API cannot express (a signed
	// URL, say). Leave those alone rather than silently dropping credentials.
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 4 {
		return "", "", "", false
	}
	for _, p := range parts[:4] { // owner, name, ref, and at least one path segment
		if p == "" {
			return "", "", "", false
		}
	}
	return parts[0] + "/" + parts[1], parts[2], strings.Join(parts[3:], "/"), true
}

// composeImages returns the upstream compose's service → image map for u,
// interpolating `${VAR}` image tags against vars.
//
// The parsed body is cached by u. A resolved URL embeds the release tag and a
// tag's compose file is immutable, so a hit is always correct and a new release
// simply mints a new key. That collapses the 15-minute imagecheck pass, the
// update's re-resolve, and the post-update ForceRecheck into ONE fetch per
// release. The tag itself is still re-resolved every time (a cheap authenticated
// API call), so freshness is never traded away for the cache.
func (g *githubClient) composeImages(ctx context.Context, u string, vars map[string]string) (map[string]string, error) {
	if tmpl, ok := g.cachedCompose(u); ok {
		return interpolateImages(tmpl, vars), nil
	}

	var (
		body []byte
		err  error
	)
	if repo, ref, path, ok := parseRawGithubURL(u); ok {
		body, err = g.rawContents(ctx, repo, path, ref)
	} else {
		body, err = g.getBytes(ctx, u, "", false)
	}
	if err != nil {
		return nil, err
	}

	var doc composeImagesDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse upstream compose: %w", err)
	}
	tmpl := make(map[string]string, len(doc.Services))
	for name, s := range doc.Services {
		if s.Image != "" {
			tmpl[name] = s.Image
		}
	}
	g.storeCompose(u, tmpl)
	// One line per real network fetch — i.e. once per upstream release, not once
	// per pass. The cache-hit path stays silent.
	slog.Info("upstream compose fetched", "url", u, "services", len(tmpl))
	return interpolateImages(tmpl, vars), nil
}

func (g *githubClient) cachedCompose(u string) (map[string]string, bool) {
	g.composeMu.Lock()
	defer g.composeMu.Unlock()
	tmpl, ok := g.composeCache[u]
	return tmpl, ok
}

func (g *githubClient) storeCompose(u string, tmpl map[string]string) {
	g.composeMu.Lock()
	defer g.composeMu.Unlock()
	if len(g.composeCache) >= composeCacheMax {
		clear(g.composeCache)
	}
	g.composeCache[u] = tmpl
}

// interpolateImages resolves each cached image template against vars into a FRESH
// map. The cache holds uninterpolated templates, so it stays vars-independent and
// callers never share (or mutate) a cached map.
func interpolateImages(tmpl, vars map[string]string) map[string]string {
	out := make(map[string]string, len(tmpl))
	for name, img := range tmpl {
		out[name] = interpolateComposeVars(img, vars)
	}
	return out
}

// ---------------------------------------------------------------------------
// buildAgentResolver — genmon / frigate-custom. Building stays in build-agent
// (agent-kit lockstep rule). Faithfully wiring the trigger requires the stack's
// full build descriptor (build.json) + repo/ref — exactly what ansible's
// build_genmon_image.yaml / deploy_custom_frigate_version.yaml already assemble —
// so for now this is a DOCUMENTED deferral, not a guessed half-implementation.
//
// genmon will auto-detect to github-release once its Dockerfile carries the OCI
// source/version labels (master plan), so STATUS works without this; only the
// actual rebuild stays in ansible until a follow-up threads the descriptor here.
// ---------------------------------------------------------------------------

type buildAgentResolver struct {
	e *engine
	o strategyOverride
}

func (r *buildAgentResolver) kind() string { return "build-agent" }

func (r *buildAgentResolver) plan(_ context.Context, _ *types.Project, log func(string)) (map[string]string, error) {
	log("build-agent resolver is not wired yet")
	return nil, fmt.Errorf("build-agent resolver not yet wired: build %q via ansible (build_genmon_image.yaml / deploy_custom_frigate_version.yaml); see handoff-phase35", orDefault(r.o.BuildDescriptor, r.o.Key))
}
