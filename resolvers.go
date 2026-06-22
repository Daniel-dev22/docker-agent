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
	"io"
	"net/http"
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
		info, _ := r.e.docker.inspectImage(ctx, svc.Image) // best-effort (may be unpulled)
		synthC := ContainerStatus{ComposeProject: project.Name, ComposeService: name, Image: svc.Image}
		strat := ic.resolveStrategy(synthC, info, ref)

		chk := ic.checkUnit(ctx, synthC)
		switch chk.ImageStatus {
		case statusOutdated:
			// registry-digest = same tag, newer digest → pull handles it (no rewrite).
			if strat.source == "registry-digest" {
				log(fmt.Sprintf("%s: newer digest for %s (pull)", name, svc.Image))
				continue
			}
			if chk.LatestImageVersion != "" && chk.LatestImageVersion != svc.Image {
				targets[name] = chk.LatestImageVersion
				log(fmt.Sprintf("%s: %s → %s (%s)", name, svc.Image, chk.LatestImageVersion, chk.VersionSource))
			}
		case statusUnknown:
			if chk.Error != "" {
				log(fmt.Sprintf("%s: version unknown (%s) — pull", name, chk.Error))
			}
		}
	}
	return targets, nil
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
	tag, err := ic.github.latestRelease(rctx, releaseQuery{Repo: repo, ReleaseType: orDefault(r.o.ReleaseType, "latest")})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("upstream-compose: latest release: %w", err)
	}
	tmpl := r.o.UpstreamCompose
	if tmpl == "" {
		tmpl = "https://raw.githubusercontent.com/" + repo + "/{tag}/docker/docker-compose.yml"
	}
	url := strings.ReplaceAll(tmpl, "{tag}", tag)
	log(fmt.Sprintf("immich: latest release %s — reading %s", tag, url))

	// immich's compose templates the app images as ${IMMICH_VERSION:-release}; pin
	// them to the concrete release tag (the whole point of the coupled update) so
	// the rewrite is a real version, not the floating `release` tag.
	upstream, err := fetchComposeImages(ctx, url, map[string]string{"IMMICH_VERSION": tag})
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

// fetchComposeImages downloads a compose file and returns service → image,
// interpolating `${VAR}` image tags against vars. Plain public HTTP
// (raw.githubusercontent) — no auth needed.
func fetchComposeImages(ctx context.Context, url string, vars map[string]string) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 20 * time.Second, Transport: pooledTransport(false)}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var doc composeImagesDoc
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse upstream compose: %w", err)
	}
	out := make(map[string]string, len(doc.Services))
	for name, s := range doc.Services {
		if s.Image != "" {
			out[name] = interpolateComposeVars(s.Image, vars)
		}
	}
	return out, nil
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
