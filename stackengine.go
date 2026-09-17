package main

// Stack-update engine — ONE shared pipeline for "bring this stack up to date"
// (the only pluggable step is image resolution, resolvers.go), driven as a long
// async compose job (streamed logs, durable events, reconcile) like any other op.
//
// updateProject pipeline:
//   1. resolve  → target {service: imageRef} (resolvers.go).
//   2. snapshot → pre-update container IDs + per-service image IDs (the rollback
//      baseline).
//   3. apply    → mutate the in-memory project + persist tags to on-host
//      compose/.env (reversible per service — tagwrite.go).
//   4. pull + up --force-recreate (in-process compose-v2; compose.go).
//   5. health-wait — swap detection + health poll + crash-loop (healthwait.go).
//   6. rollback on failure — redeploy the snapshot image IDs (pull=never), scope
//      per-container (default) or whole-stack; revert persisted tags.
//   7. reconcile networks (netreconcile.go).
//
// The inverted compose `Done(op, err)` bool (see compose.go) bites here too:
// every step's verdict comes from the returned error, never compose's flag.

import (
	"context"
	"fmt"
	"os"
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

// slowHostClasses are node-name classes that get the longer health-wait budget
// because their storage/CPU makes a recreate genuinely slower (the default,
// "pi", is a single-board computer on an SD card). It is a CONVENTION over the
// node name, not a measurement: node names are "<class><ordinal>" (e.g. "pi01",
// "nuc02"), so the class is the name minus its trailing digits. Override with
// DOCKER_SLOW_HOST_CLASSES (comma-separated; empty string = no slow classes).
func slowHostClasses() []string {
	if v, ok := os.LookupEnv("DOCKER_SLOW_HOST_CLASSES"); ok {
		return splitCSV(v)
	}
	return []string{"pi"}
}

// healthTimeouts returns the (swap, health) budget for this node: the longer pair
// on a slow host class, the standard pair otherwise. Both are env-overridable
// outright with DOCKER_HEALTH_SWAP_TIMEOUT / DOCKER_HEALTH_TIMEOUT.
func (e *engine) healthTimeouts() (swap, health time.Duration) {
	s, h := 120*time.Second, 180*time.Second
	class := strings.TrimRight(e.cfg.NodeName, "0123456789")
	for _, slow := range slowHostClasses() {
		if strings.EqualFold(class, slow) {
			s, h = 300*time.Second, 360*time.Second
			break
		}
	}
	return getEnvDuration("DOCKER_HEALTH_SWAP_TIMEOUT", s), getEnvDuration("DOCKER_HEALTH_TIMEOUT", h)
}

// healthBudgetReserve is the slice of the whole-op budget (composeOpTimeout) held
// back for the phases that run BEFORE the health wait — resolve, persist, pull and
// `up`. A health budget allowed to consume the whole op budget is not a longer
// health wait: the op deadline simply fires first, and it fires during the health
// phase, which is the one place a timeout costs us the rollback.
//
// It is a heuristic, not a bound: nothing limits the pull, so a multi-GB image on a
// slow link can still overrun it. It shrinks the common failure, it does not remove
// it. On a small op budget the reserve is taken proportionally instead (see
// clampBudget) so it can never leave nothing behind.
const healthBudgetReserve = 5 * time.Minute

// rollbackSlack is the head-room added to the rollback's own deadline on top of the
// swap+health budget, covering the disk revert, the compose reload and the `up`. It
// mirrors healthBudgetReserve because the rollback runs the same phases.
const rollbackSlack = healthBudgetReserve

// minRollbackDeadline floors the rollback's total budget.
//
// The deadline is derived from the operator's own numbers, which cuts BOTH ways: a
// deliberately short budget ("fail this stack fast") would otherwise leave the
// rollback a few seconds to revert the tags, reload the project and force-recreate
// every container. Being cancelled part-way through that is the worst available
// outcome — revertDisk has already rewritten the on-disk tags, so disk and runtime
// disagree, which is the exact divergence persistTargets exists to prevent. A
// rollback is recovery, so it gets a floor no configuration can take away.
const minRollbackDeadline = 10 * time.Minute

// failedLogDumpBudget bounds the post-failure container log dump.
const failedLogDumpBudget = 90 * time.Second

// rollbackDeadline is the total wall clock a rollback may take.
func rollbackDeadline(swap, health time.Duration) time.Duration {
	d := swap + health + rollbackSlack
	if d < minRollbackDeadline {
		return minRollbackDeadline
	}
	return d
}

// minHealthBudget is the floor the clamp may never cross.
//
// A zero health budget is not "a short wait" — `waitForHealth` computes
// `deadline := start.Add(healthTimeout)`, so zero means the deadline is already in
// the past and the loop runs exactly ONE poll. A container that has just been
// force-recreated reports `starting`, which is neither healthy nor none, so EVERY
// stack with a HEALTHCHECK fails instantly and rolls back — and the rollback, given
// the same zero, then reports itself unhealthy too. An operator who RAISED a budget
// would break every deploy on the host. So the clamp gives up swap time instead,
// and never returns less than this.
const minHealthBudget = 30 * time.Second

// maxBudget bounds any single budget the agent will honour, whatever the source.
//
// The router validates the strategy row, but it is NOT a gate on the request path:
// the agent is reached in-process over the docker network with no bearer or mTLS
// (see docker-agent/CLAUDE.md), so `health_timeout_s` on a compose op arrives
// unvalidated. Without a bound here, a large value overflows time.Duration into a
// NEGATIVE deadline — which sails past the clamp (a negative sum is never over
// budget), expires immediately, and aborts the rollback AFTER the disk tags have
// been reverted. That leaves disk and runtime disagreeing, which is the exact
// divergence persistTargets exists to prevent.
const maxBudget = 24 * time.Hour

// clampBudget fits swap+health inside the op budget, preserving the health half.
//
// Returns the pair plus whether it had to clamp. Health is the part an operator
// actually meant to lengthen, so swap yields first and health only shrinks once
// swap is at zero — and never below minHealthBudget.
func (e *engine) clampBudget(swap, health time.Duration) (time.Duration, time.Duration, bool) {
	if e.composeOpTimeout <= 0 {
		return swap, health, false // no op deadline to fit inside
	}
	// On a small op budget a flat 5m reserve would leave nothing (or a negative
	// budget) and the clamp would silently do nothing at all — which is the
	// configuration where it matters most. Take a third instead.
	reserve := healthBudgetReserve
	if reserve > e.composeOpTimeout/3 {
		reserve = e.composeOpTimeout / 3
	}
	budget := e.composeOpTimeout - reserve
	if budget <= 0 || swap+health <= budget {
		return swap, health, false
	}
	if health > budget-minHealthBudget {
		health = budget - minHealthBudget
	}
	if health < minHealthBudget {
		health = minHealthBudget
	}
	if swap > budget-health {
		swap = budget - health
	}
	if swap < 0 {
		swap = 0
	}
	return swap, health, true
}

// effectiveHealthTimeouts resolves the (swap, health) budget for one update.
//
// Precedence is REQUEST > project row > node-class default, and this is the only
// place it is decided — updateProject and rollback both call it, so a stack given a
// long boot budget is verified against that same budget on the way back. Getting
// that wrong is not cosmetic: rollback re-deriving the node default would declare a
// perfectly good rollback "still unhealthy" on every slow stack.
//
// `phase` labels the job log line so a failed update's forward and rollback budgets
// are told apart. Every announcement prints the RESOLVED values, because a line
// that says only "clamped" tells an operator a number changed without saying to
// what — a silent cap wearing a log line.
func (e *engine) effectiveHealthTimeouts(j *Job, meta resolveMeta, phase string) (swap, health time.Duration) {
	swap, health = e.healthTimeouts()
	swapSrc, healthSrc := "node-class", "node-class"
	if v, ok := budgetOf(meta.swapTimeoutS); ok {
		swap, swapSrc = v, "project"
	}
	if v, ok := budgetOf(meta.healthTimeoutS); ok {
		health, healthSrc = v, "project"
	}
	if j != nil {
		if v, ok := budgetOf(j.swapTimeoutS); ok {
			swap, swapSrc = v, "request"
		}
		if v, ok := budgetOf(j.healthTimeoutS); ok {
			health, healthSrc = v, "request"
		}
	}

	swap, health, clamped := e.clampBudget(swap, health)
	if clamped {
		// The clamped value is no longer what its source asked for, so stop
		// attributing it to that source — saying "health 0s (project)" blames a row
		// that asked for 900s.
		swapSrc, healthSrc = swapSrc+", clamped", healthSrc+", clamped"
	}
	if j != nil && (clamped || swapSrc != "node-class" || healthSrc != "node-class") {
		j.appendLine(fmt.Sprintf("%s health budget: swap %s (%s), health %s (%s)",
			phase, swap, swapSrc, health, healthSrc))
	}
	return swap, health
}

// budgetOf converts a seconds value from a row or a request into a duration,
// reporting whether it is a usable override.
//
// Zero and negative are "not set", never "set to zero" — a zero that overrode would
// disable the health wait entirely (see minHealthBudget). Anything past maxBudget is
// refused rather than truncated at the far end, where it would already have
// overflowed into a negative duration.
func budgetOf(seconds int) (time.Duration, bool) {
	if seconds <= 0 {
		return 0, false
	}
	d := time.Duration(seconds) * time.Second
	if d <= 0 || d > maxBudget { // d <= 0 catches the multiplication overflowing
		return 0, false
	}
	return d, true
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
	// Re-check image status NOW (bypassing the TTL coalesce) so the just-deployed
	// image's "updated" status reaches the fleet + discovery feed within seconds —
	// otherwise the controller's smart-routing keeps seeing the stale "outdated" and
	// re-targets this stack on the next action.
	if e.images != nil {
		e.images.ForceRecheck()
	}
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

	// 2. resolve targets (read-only plan).
	//
	// Pull the central overrides BEFORE selecting the resolver. selectResolver reads
	// the cached override to build `meta` (health budget, rollback scope, health
	// excludes), while registryResolver.plan refreshes the cache itself — so without
	// this, one update resolved its TARGETS from a fresh fetch and its POLICY from
	// whatever the last periodic pass happened to hold. The visible symptom is the
	// worst kind: an operator raises a stack's health timeout, runs the update
	// immediately, and it still fails on the old budget — indistinguishable from the
	// feature not working. One fetch here makes both halves of the row agree.
	if e.images != nil {
		e.images.refreshOverrides(ctx)
	}
	resolver, meta := e.selectResolver(name, j.overrideImage, j.overrideService)
	j.appendLine("resolver: " + resolver.kind() + ", rollback-scope: " + meta.rollbackScope)
	targets, err := resolver.plan(ctx, project, j.appendLine)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}

	// Gate the update: a coupled/special project (its plan is the whole truth) is
	// only updated when its plan changes ≥1 service — an empty plan is a clean
	// no-op, never a needless force-recreate. Registry projects keep their
	// existing pull+recreate (a registry-digest "newer digest" yields no target
	// rewrite, so an empty target set there does NOT mean "nothing to pull").
	if resolver.kind() != "registry" && len(targets) == 0 {
		j.appendLine("up to date — nothing to deploy")
		return nil
	}

	// 3. apply targets in-memory + persist to disk (reversible per service).
	applyImages(project, targets)
	diskOld, perr := e.persistTargets(j, entry, targets) // service → prior on-disk image
	if perr != nil {
		// A persist failure would leave running state ahead of disk (reverts on the
		// next restart / manual `compose up`). Revert what we wrote and fail loudly.
		e.revertDisk(j, entry, diskOld)
		return fmt.Errorf("persist: %w", perr)
	}

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
		e.rollback(parentCtx, j, entry, meta, base, base.allServices(), diskOld, fleetTrigger)
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
	swapTO, healthTO := e.effectiveHealthTimeouts(j, meta, "update")
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
		// parentCtx, not ctx. The op deadline is most likely to expire DURING the
		// health wait, and this dump is the only diagnosis that ever reaches the
		// job log — the container's own output. On the 2026-08-29 frigate failure
		// it is what identified the cause (an s6 fix-ownership pass over /config);
		// on the op context it would have printed
		// "(logs unavailable: context deadline exceeded)" for every container and
		// the incident would have been unexplainable.
		logCtx, logCancel := context.WithTimeout(parentCtx, failedLogDumpBudget)
		e.dumpFailedLogs(logCtx, j, res.unhealthy)
		logCancel()
		if freshDeploy {
			j.appendLine("fresh deploy unhealthy — no prior version to roll back to")
			e.revertDisk(j, entry, diskOld)
			return fmt.Errorf("health: %w", herr)
		}
		scope := base.servicesForContainers(res.unhealthy)
		if meta.rollbackScope == "whole-stack" {
			scope = base.allServices()
		}
		e.rollback(parentCtx, j, entry, meta, base, scope, diskOld, fleetTrigger)
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
// the prior on-disk value per service (for a reversible rollback). A persistence
// failure is fatal: returning an error lets the caller revert the partial writes and
// fail the update, rather than deploy a tag that disk would revert on the next
// `compose up`. The returned map holds whatever was written before the failure.
func (e *engine) persistTargets(j *Job, entry ProjectEntry, targets map[string]string) (map[string]priorImage, error) {
	old := map[string]priorImage{}
	for svc, img := range targets {
		prev, err := setServiceImage(entry, svc, img)
		if err != nil {
			return old, fmt.Errorf("persist %s tag: %w", svc, err)
		}
		old[svc] = prev
	}
	return old, nil
}

// revertDisk restores each persisted service's prior on-disk image (pull-failure
// path, and the per-service half of a rollback).
func (e *engine) revertDisk(j *Job, entry ProjectEntry, diskOld map[string]priorImage) {
	for svc, prev := range diskOld {
		if err := restoreServiceImage(entry, svc, prev); err != nil {
			j.appendLine(fmt.Sprintf("warn: revert %s tag failed: %v", svc, err))
		}
	}
}

// rollback redeploys the scope services pinned to their pre-update image IDs
// (pull=never), then re-health-waits. Scope per-container rolls back only the
// unhealthy services (leaving healthy updated ones); whole-stack rolls back
// everything, which is what a version-locked (coupled) stack needs.
//
// parentCtx is the JOB context, never the op context. The op context carries
// composeOpTimeout, and the single most likely moment for that deadline to expire
// is during a long health wait — i.e. exactly when a rollback is about to be
// needed. Running the rollback on that context meant the recovery was cancelled
// before it started and the stack was left on the image that had just failed its
// health check, with the job reporting a health failure and no revert. So the
// rollback is detached from cancellation entirely (WithoutCancel) and given its own
// bounded deadline: it is the safety action, and it must be allowed to finish even
// when the operator cancelled or the op budget ran out.
func (e *engine) rollback(parentCtx context.Context, j *Job, entry ProjectEntry, meta resolveMeta, base projectBaseline, scope []string, diskOld map[string]priorImage, fleetTrigger func()) {
	if len(scope) == 0 {
		j.appendLine("rollback: no services in scope")
		return
	}
	j.appendLine("rolling back: " + strings.Join(scope, ", "))

	swapTO, healthTO := e.effectiveHealthTimeouts(j, meta, "rollback")
	// parentCtx, NOT the op context. The op context carries composeOpTimeout, and the
	// most likely moment for that deadline to expire is during a long health wait —
	// exactly when a rollback is about to be needed. Deriving from it cancelled the
	// recovery before it started and left the stack on the image that had just
	// failed, with the job reporting a health failure and no revert.
	//
	// It is deliberately NOT context.WithoutCancel: an operator who cancels the job
	// must still be able to stop it. Detaching from cancellation as well made
	// POST /v1/jobs/:id/cancel return 200 while the engine kept force-recreating
	// containers for minutes with no way to abort, and turned a cancel during `up`
	// from "stop" into "silently redeploy the previous images".
	ctx, cancel := context.WithTimeout(parentCtx, rollbackDeadline(swapTO, healthTO))
	defer cancel()

	// Revert the persisted tags for the scoped services first, so disk reflects
	// the rollback (healthy updated services keep their new persisted tags).
	for _, svc := range scope {
		if prev, ok := diskOld[svc]; ok {
			if err := restoreServiceImage(entry, svc, prev); err != nil {
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

	// Verify the rollback came up healthy (best-effort — log only). It uses the
	// SAME budget and the SAME health subset/excludes as the forward deploy: a
	// stack slow enough to need a long budget is equally slow coming back, and a
	// shell-less container that cannot report health forward cannot report it
	// backward either. Re-deriving either here reported healthy rollbacks as
	// "still unhealthy" and burned the whole budget doing it.
	post := e.snapshotProject(ctx, entry.Name)
	rhw := healthWaitParams{
		containers:       post.containerNames(),
		previousIDs:      base.previousIDs(),
		excluded:         toSet(meta.healthExcludes),
		onlyHealth:       meta.healthContainers,
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
// log, so the failure is diagnosable from the job stream alone.
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
