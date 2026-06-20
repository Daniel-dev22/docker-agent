package main

// Stack-update engine (Phase 3.5) — the Portainer fold-in. The Go replacement
// for manage_portainer_stack_update.yaml's orchestration: ONE shared pipeline
// (the only pluggable step is image resolution, resolvers.go), driven as a long
// async compose job (streamed logs, durable events, reconcile) like any other op.
//
// updateProject pipeline:
//   1. resolve  → target {service: imageRef} (resolvers.go).
//   2. snapshot → pre-update container IDs + per-service image IDs (rollback
//      baseline = the playbook's monitored_data).
//   3. apply    → mutate the in-memory project + persist tags to on-host
//      compose/.env (reversible per service — tagwrite.go).
//   4. pull + up --force-recreate (in-process compose-v2; compose.go).
//   5. health-wait — swap detection + health poll + crash-loop (healthwait.go).
//   6. rollback on failure — redeploy the snapshot image IDs (pull=never), scope
//      per-container (default) or whole-stack (immich); revert persisted tags.
//   7. reconcile networks + clear status (netreconcile.go).
//
// The inverted compose `Done(op, err)` bool (Phase 2 trap) bites here too: every
// step's verdict comes from the returned error, never compose's success flag.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
)

// containerBaseline is one pre-update container (the rollback anchor).
type containerBaseline struct {
	name    string
	id      string
	service string
	imageID string
}

// projectBaseline is the pre-update state of a project's containers.
type projectBaseline struct {
	containers []containerBaseline
}

func (b projectBaseline) containerNames() []string {
	out := make([]string, 0, len(b.containers))
	for _, c := range b.containers {
		out = append(out, c.name)
	}
	sort.Strings(out)
	return out
}

func (b projectBaseline) previousIDs() map[string]string {
	m := make(map[string]string, len(b.containers))
	for _, c := range b.containers {
		m[c.name] = c.id
	}
	return m
}

// serviceImageIDs maps each service to the image ID it was running before the
// update (first container wins) — the exact image a rollback pins to.
func (b projectBaseline) serviceImageIDs() map[string]string {
	m := map[string]string{}
	for _, c := range b.containers {
		if c.service == "" || c.imageID == "" {
			continue
		}
		if _, ok := m[c.service]; !ok {
			m[c.service] = c.imageID
		}
	}
	return m
}

// servicesForContainers returns the distinct services owning the given container
// names (per-container rollback scope).
func (b projectBaseline) servicesForContainers(names []string) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range b.containers {
		if want[c.name] && c.service != "" && !seen[c.service] {
			seen[c.service] = true
			out = append(out, c.service)
		}
	}
	return out
}

// allServices returns every distinct service in the baseline (whole-stack scope).
func (b projectBaseline) allServices() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range b.containers {
		if c.service != "" && !seen[c.service] {
			seen[c.service] = true
			out = append(out, c.service)
		}
	}
	return out
}

// resolveEntry finds the project entry to act on (durable registry, else live).
func (e *engine) resolveEntry(ctx context.Context, name string) (ProjectEntry, bool) {
	if entry, ok := e.projects.get(name); ok {
		return entry, true
	}
	if _, live, err := e.docker.snapshot(ctx); err == nil {
		return e.projects.resolve(name, live)
	}
	return ProjectEntry{}, false
}

// snapshotProject captures the project's current containers (rollback baseline).
func (e *engine) snapshotProject(ctx context.Context, name string) projectBaseline {
	var b projectBaseline
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	containers, _, err := e.docker.snapshot(sctx)
	cancel()
	if err != nil {
		return b
	}
	for _, c := range containers {
		if c.ComposeProject != name {
			continue
		}
		b.containers = append(b.containers, containerBaseline{
			name: c.Name, id: c.ID, service: c.ComposeService, imageID: c.ImageID,
		})
	}
	return b
}

// healthTimeouts returns (swap, health) per host class — Pi gets the longer waits
// (slow SD-card I/O), mirroring manage_portainer_stack_update.yaml. Env-overridable.
func (e *engine) healthTimeouts() (swap, health time.Duration) {
	s, h := 120*time.Second, 180*time.Second
	if strings.EqualFold(e.cfg.NodeName, "pi") {
		s, h = 300*time.Second, 360*time.Second
	}
	return getEnvDuration("DOCKER_HEALTH_SWAP_TIMEOUT", s), getEnvDuration("DOCKER_HEALTH_TIMEOUT", h)
}

// runUpdate is the engine entry point for op=update (dispatched in engine.run).
func (e *engine) runUpdate(ctx context.Context, j *Job, fleetTrigger func()) {
	if e.compose == nil {
		j.markFailed("compose backend unavailable")
		return
	}
	name := j.snapshot().Project
	if name == "" {
		j.markFailed("no target project")
		return
	}
	entry, ok := e.resolveEntry(ctx, name)
	if !ok {
		j.markFailed("unknown project: " + name)
		return
	}

	opCtx := ctx
	if e.composeOpTimeout > 0 {
		var cancel context.CancelFunc
		opCtx, cancel = context.WithTimeout(ctx, e.composeOpTimeout)
		defer cancel()
	}

	if err := e.updateProject(opCtx, ctx, j, entry, fleetTrigger); err != nil {
		if ctx.Err() != nil {
			j.appendLine("cancelled")
			j.markFailed("cancelled: " + err.Error())
		} else {
			j.appendLine("error: " + err.Error())
			j.markFailed(err.Error())
		}
		fleetTrigger()
		return
	}
	j.appendLine("update " + name + " ok")
	j.markCompleted()
	fleetTrigger()
}

// updateProject runs the full pipeline. parentCtx is the un-timed-out job context
// (used to tell cancellation apart from the op timeout). Returns an error on any
// failure AFTER a best-effort rollback.
func (e *engine) updateProject(ctx, parentCtx context.Context, j *Job, entry ProjectEntry, fleetTrigger func()) error {
	name := entry.Name

	project, err := e.compose.loadProject(ctx, entry)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}

	// 1. baseline snapshot (pre-update container IDs + per-service image IDs).
	base := e.snapshotProject(ctx, name)

	// 2. resolve targets.
	resolver, meta := e.selectResolver(name, j.overrideImage, j.overrideService)
	j.appendLine("resolver: " + resolver.kind() + ", rollback-scope: " + meta.rollbackScope)
	targets, err := resolver.resolve(ctx, j, project)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	// 3. apply targets in-memory + persist to disk (reversible per service).
	applyImages(project, targets)
	diskOld := e.persistTargets(j, entry, targets) // service → prior on-disk image

	// 4. pull + up --force-recreate.
	j.appendLine("pulling images")
	if err := e.compose.pullProject(ctx, j, project, fleetTrigger); err != nil {
		// No containers recreated yet — just revert any persisted tags.
		e.revertDisk(j, entry, diskOld)
		return fmt.Errorf("pull: %w", err)
	}
	fleetTrigger()
	j.appendLine("deploying (up --force-recreate)")
	if err := e.compose.upProject(ctx, j, project, api.RecreateForce, fleetTrigger); err != nil {
		e.rollback(ctx, j, entry, base, base.allServices(), diskOld, fleetTrigger)
		return fmt.Errorf("up: %w", err)
	}
	fleetTrigger()

	// 5. health-wait (swap detection + health poll).
	monitored := base.containerNames()
	freshDeploy := len(monitored) == 0
	if freshDeploy {
		// Stack was down — re-snapshot the now-running containers, no swap phase.
		base = e.snapshotProject(ctx, name)
		monitored = base.containerNames()
	}
	if len(monitored) == 0 {
		j.appendLine("no containers to health-check (nothing started?)")
		return fmt.Errorf("no containers running after up")
	}
	swapTO, healthTO := e.healthTimeouts()
	hw := healthWaitParams{
		containers:       monitored,
		excluded:         toSet(meta.healthExcludes),
		onlyHealth:       meta.healthContainers,
		swapTimeout:      swapTO,
		healthTimeout:    healthTO,
		restartThreshold: 3,
	}
	if !freshDeploy {
		hw.previousIDs = base.previousIDs()
	}
	res, herr := e.healthWait(ctx, j, hw)
	if herr != nil {
		e.dumpFailedLogs(ctx, j, res.unhealthy)
		if freshDeploy {
			j.appendLine("fresh deploy unhealthy — no prior version to roll back to")
			e.revertDisk(j, entry, diskOld)
			return fmt.Errorf("health: %w", herr)
		}
		scope := base.servicesForContainers(res.unhealthy)
		if meta.rollbackScope == "whole-stack" {
			scope = base.allServices()
		}
		e.rollback(ctx, j, entry, base, scope, diskOld, fleetTrigger)
		return fmt.Errorf("health: %w", herr)
	}

	// 6. reconcile networks (best-effort — never fails the update).
	e.reconcileNetworks(ctx, j, entry, project, fleetTrigger)
	return nil
}

// applyImages mutates the in-memory project so pull+up deploy the resolved tags.
func applyImages(project *types.Project, targets map[string]string) {
	for svc, img := range targets {
		if s, ok := project.Services[svc]; ok {
			s.Image = img
			project.Services[svc] = s
		}
	}
}

// persistTargets writes each resolved tag to the on-host compose/.env and returns
// the prior on-disk value per service (for a reversible rollback). Best-effort:
// a persistence failure is logged, not fatal (the in-memory deploy still proceeds).
func (e *engine) persistTargets(j *Job, entry ProjectEntry, targets map[string]string) map[string]string {
	old := map[string]string{}
	for svc, img := range targets {
		prev, err := setServiceImage(entry, svc, img)
		if err != nil {
			j.appendLine(fmt.Sprintf("warn: persist %s tag failed: %v", svc, err))
			continue
		}
		old[svc] = prev
	}
	return old
}

// revertDisk restores each persisted service's prior on-disk image (pull-failure
// path, and the per-service half of a rollback).
func (e *engine) revertDisk(j *Job, entry ProjectEntry, diskOld map[string]string) {
	for svc, prev := range diskOld {
		if _, err := setServiceImage(entry, svc, prev); err != nil {
			j.appendLine(fmt.Sprintf("warn: revert %s tag failed: %v", svc, err))
		}
	}
}

// rollback redeploys the scope services pinned to their pre-update image IDs
// (pull=never), then re-health-waits. Faithful to the playbook rescue block:
// per-container rolls back only the unhealthy services (leaving healthy updated
// ones), whole-stack rolls back everything (immich's coupled services).
func (e *engine) rollback(ctx context.Context, j *Job, entry ProjectEntry, base projectBaseline, scope []string, diskOld map[string]string, fleetTrigger func()) {
	if len(scope) == 0 {
		j.appendLine("rollback: no services in scope")
		return
	}
	j.appendLine("rolling back: " + strings.Join(scope, ", "))

	// Revert the persisted tags for the scoped services first, so disk reflects
	// the rollback (healthy updated services keep their new persisted tags).
	for _, svc := range scope {
		if prev, ok := diskOld[svc]; ok {
			if _, err := setServiceImage(entry, svc, prev); err != nil {
				j.appendLine(fmt.Sprintf("warn: revert %s tag failed: %v", svc, err))
			}
		}
	}

	project, err := e.compose.loadProject(ctx, entry)
	if err != nil {
		j.appendLine("rollback load failed: " + err.Error())
		return
	}
	// Pin the scoped services to the exact image IDs they ran before — handles
	// both retagged and moving-tag (digest) services — and disable pull.
	sid := base.serviceImageIDs()
	pinned := 0
	for _, svc := range scope {
		id := sid[svc]
		if id == "" {
			continue
		}
		if s, ok := project.Services[svc]; ok {
			s.Image = id
			s.PullPolicy = types.PullPolicyNever
			project.Services[svc] = s
			pinned++
		}
	}
	if pinned == 0 {
		j.appendLine("rollback: no prior image IDs to pin — leaving current state")
		return
	}
	if err := e.compose.upProject(ctx, j, project, api.RecreateForce, fleetTrigger); err != nil {
		j.appendLine("rollback deploy failed: " + err.Error())
		return
	}
	fleetTrigger()

	// Verify the rollback came up healthy (best-effort — log only).
	swapTO, healthTO := e.healthTimeouts()
	post := e.snapshotProject(ctx, entry.Name)
	rhw := healthWaitParams{
		containers:       post.containerNames(),
		previousIDs:      base.previousIDs(),
		swapTimeout:      swapTO,
		healthTimeout:    healthTO,
		restartThreshold: 3,
	}
	if _, herr := e.healthWait(ctx, j, rhw); herr != nil {
		j.appendLine("rollback still unhealthy: " + herr.Error())
	} else {
		j.appendLine("rollback healthy")
	}
}

// dumpFailedLogs appends the tail of each unhealthy container's logs to the job
// log (the playbook's failed_container_logs, but inline in the job stream).
func (e *engine) dumpFailedLogs(ctx context.Context, j *Job, unhealthy []string) {
	for _, name := range unhealthy {
		j.appendLine("--- logs: " + name + " ---")
		lctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		rc, err := e.docker.containerLogsTail(lctx, name, "80")
		if err != nil {
			cancel()
			j.appendLine("  (logs unavailable: " + err.Error() + ")")
			continue
		}
		for _, ln := range rc {
			j.appendLine("  " + ln)
		}
		cancel()
	}
}

// toSet builds a name→true set from a slice.
func toSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
