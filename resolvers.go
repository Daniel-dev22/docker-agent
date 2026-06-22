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
type ImageResolver interface {
	resolve(ctx context.Context, j *Job, project *types.Project) (map[string]string, error)
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

func (r *registryResolver) resolve(ctx context.Context, j *Job, project *types.Project) (map[string]string, error) {
	ic := r.e.images
	if ic == nil {
		j.appendLine("no image checker — pull-only update (no version resolution)")
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
				j.appendLine(fmt.Sprintf("%s: newer digest for %s (pull)", name, svc.Image))
				continue
			}
			if chk.LatestImageVersion != "" && chk.LatestImageVersion != svc.Image {
				targets[name] = chk.LatestImageVersion
				j.appendLine(fmt.Sprintf("%s: %s → %s (%s)", name, svc.Image, chk.LatestImageVersion, chk.VersionSource))
			}
		case statusUnknown:
			if chk.Error != "" {
				j.appendLine(fmt.Sprintf("%s: version unknown (%s) — pull", name, chk.Error))
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

func (r *overrideResolver) resolve(_ context.Context, j *Job, project *types.Project) (map[string]string, error) {
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
	if _, ok := project.Services[svc]; !ok {
		return nil, fmt.Errorf("override resolver: service %q not in project %q", svc, project.Name)
	}
	j.appendLine(fmt.Sprintf("override: %s → %s", svc, r.image))
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

func (r *upstreamComposeResolver) resolve(ctx context.Context, j *Job, project *types.Project) (map[string]string, error) {
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
	j.appendLine(fmt.Sprintf("immich: latest release %s — reading %s", tag, url))

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
		if img != svc.Image {
			targets[name] = img
			j.appendLine(fmt.Sprintf("%s: %s → %s (upstream %s)", name, svc.Image, img, upName))
		}
	}
	if len(targets) == 0 {
		j.appendLine("immich: already on the latest coupled image set")
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

func (r *buildAgentResolver) resolve(_ context.Context, j *Job, _ *types.Project) (map[string]string, error) {
	j.appendLine("build-agent resolver is not wired yet")
	return nil, fmt.Errorf("build-agent resolver not yet wired: build %q via ansible (build_genmon_image.yaml / deploy_custom_frigate_version.yaml); see handoff-phase35", orDefault(r.o.BuildDescriptor, r.o.Key))
}
