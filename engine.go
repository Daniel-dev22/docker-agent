package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Operation names. Container-scoped lifecycle ops act on a single Target; the
// bulk variants ("container.bulk.<verb>") fan out over Targets. Compose ops and
// the stack-update op are project-scoped.
const (
	opContainerStart      = "container.start"
	opContainerStop       = "container.stop"
	opContainerRestart    = "container.restart"
	opContainerRemove     = "container.remove"
	opContainerKill       = "container.kill"
	opContainerBulkPrefix = "container.bulk." // + verb (start|stop|restart|kill|remove)

	// Compose project ops — project-scoped, run via the in-process compose
	// library (compose.go). The Job's Project names the target stack.
	opComposeUp       = "up"
	opComposeDown     = "down"
	opComposePull     = "pull"
	opComposeRestart  = "restart"
	opComposeRecreate = "recreate"

	// Stack-update engine — resolves each service's target image, snapshots for
	// rollback, pulls + recreates, health-waits, and rolls back on failure.
	// Project-scoped like the compose ops, but driven by
	// stackengine.go (updateProject), not the plain compose.execute path.
	opComposeUpdate = "update"
)

// composeOps is the set of plain compose ops (compose.go) — used for dispatch +
// request validation. The stack-update op ("update") is dispatched separately
// (stackengine.go) so it is intentionally NOT in this set.
var composeOps = map[string]bool{
	opComposeUp: true, opComposeDown: true, opComposePull: true,
	opComposeRestart: true, opComposeRecreate: true,
}

// projectOps is the full set of valid POST /v1/projects/:name/op values
// (the plain compose ops plus the stack-update op) — used for request validation.
var projectOps = map[string]bool{
	opComposeUp: true, opComposeDown: true, opComposePull: true,
	opComposeRestart: true, opComposeRecreate: true, opComposeUpdate: true,
}

// engine executes a Job's operation against the docker engine, streaming output
// to the job's log ring (j.appendLine) and emitting lifecycle events via
// onChange. It holds the SDK client + the durable compose registry.
//
// Three operation families:
//   - container lifecycle (start/stop/restart/remove/kill, bulk).
//   - compose ops (up/down/pull/restart/recreate) via the compose Go library,
//     with progress events fed into j.appendLine.
//   - the stack-update engine (resolve→snapshot→pull+up→health-wait→rollback).
type engine struct {
	cfg      Config
	docker   *dockerClient
	compose  *composeBackend  // in-process compose-v2 SDK (nil if init failed)
	projects *composeRegistry // durable project index
	// images is the image-outdated checker, reused by the stack-update engine
	// for strategy resolution (central overrides + auto-detect)
	// and its registry/github clients. Set via setImageChecker after construction
	// (app.go wires it once the checker exists). nil → update falls back to a
	// pull-only deploy with no version resolution.
	images *imageChecker

	// bulkConcurrency caps simultaneous container ops in a bulk fan-out so a
	// low-power host is not thrashed by a wide fan-out.
	bulkConcurrency int
	// composeOpTimeout bounds one compose op so a wedged pull/up can't run
	// forever; cancellation still works via the job context. A job queued behind
	// another on its project waits at most this long, then runs with its own.
	composeOpTimeout time.Duration

	// locks serialises every change to one project (projectlock.go).
	locks *projectLocks
}

// setImageChecker wires the image checker into the engine for the update path
// (strategy resolution + shared registry/github clients).
func (e *engine) setImageChecker(ic *imageChecker) { e.images = ic }

func newEngine(cfg Config, dc *dockerClient, cb *composeBackend, reg *composeRegistry) *engine {
	bc := getEnvInt("DOCKER_BULK_CONCURRENCY", defaultBulkConcurrency())
	if bc < 1 {
		bc = 1
	}
	return &engine{
		cfg:              cfg,
		docker:           dc,
		compose:          cb,
		projects:         reg,
		bulkConcurrency:  bc,
		composeOpTimeout: getEnvDuration("DOCKER_COMPOSE_OP_TIMEOUT", 30*time.Minute),
		locks:            newProjectLocks(),
	}
}

// defaultBulkConcurrency derives from the (cgroup-accurate, Go 1.25) GOMAXPROCS
// and is clamped to [2,4]: a big host doesn't fan out wildly and a small one
// stays gentle.
func defaultBulkConcurrency() int {
	n := runtime.GOMAXPROCS(0)
	switch {
	case n > 4:
		return 4
	case n < 2:
		return 2
	default:
		return n
	}
}

// run drives one job to a terminal state, emitting started → (completed|failed|
// cancelled). fleetTrigger is a cheap fleet-only nudge so a container's new state
// repaints promptly between lifecycle events.
func (e *engine) run(ctx context.Context, j *Job, onChange func(JobEvent), fleetTrigger func()) {
	j.setRunning()
	onChange(EventStarted)

	op := j.snapshot().Operation
	switch {
	case op == opContainerStart, op == opContainerStop, op == opContainerRestart,
		op == opContainerRemove, op == opContainerKill:
		e.runContainerOp(ctx, j, strings.TrimPrefix(op, "container."), fleetTrigger)
	case strings.HasPrefix(op, opContainerBulkPrefix):
		e.runBulk(ctx, j, strings.TrimPrefix(op, opContainerBulkPrefix), fleetTrigger)
	case op == opComposeUpdate || composeOps[op]:
		if release, ok := e.lockProject(ctx, j, op); ok {
			if op == opComposeUpdate {
				e.runUpdate(ctx, j, fleetTrigger)
			} else {
				e.runComposeOp(ctx, j, op, fleetTrigger)
			}
			release()
		}
	default:
		j.markFailed("unsupported operation: " + op)
		slog.Warn("job with unsupported operation", "id", j.snapshot().ID, "operation", op)
	}

	switch j.snapshot().State {
	case JobCancelled:
		onChange(EventCancelled)
	case JobCompleted:
		onChange(EventCompleted)
	default:
		onChange(EventFailed)
	}
}

// lockProject takes the job's project lock for a project-scoped op, logging what
// it waits for. The wait honours the job's cancellation and gives up after
// composeOpTimeout; either way the job is left terminal and ok is false.
func (e *engine) lockProject(ctx context.Context, j *Job, op string) (release func(), ok bool) {
	snap := j.snapshot()
	wctx := ctx
	if e.composeOpTimeout > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, e.composeOpTimeout)
		defer cancel()
	}
	key := projectLockKey(ProjectEntry{Name: snap.Project})
	if entry, ok := e.resolveEntry(wctx, snap.Project); ok {
		key = projectLockKey(entry)
	}
	release, err := e.locks.acquire(wctx, key, fmt.Sprintf("job %s (%s of %s)", snap.ID, op, snap.Project), func(holder string) {
		j.appendLine(fmt.Sprintf("queued: waiting for %s, which is changing the same project directory", holder))
	})
	if err == nil {
		return release, true
	}
	if ctx.Err() != nil {
		j.appendLine("cancelled while queued")
		j.markFailed("cancelled while queued")
		return nil, false
	}
	msg := fmt.Sprintf("gave up after %s queued behind another change to project %s", e.composeOpTimeout, snap.Project)
	j.appendLine("error: " + msg)
	j.markFailed(msg)
	return nil, false
}

// runContainerOp executes a single-container lifecycle verb on j.Target.
func (e *engine) runContainerOp(ctx context.Context, j *Job, verb string, fleetTrigger func()) {
	ref := j.snapshot().Target
	if len(j.targetIDs) != 1 {
		j.markFailed("no checked target container")
		return
	}
	j.appendLine(fmt.Sprintf("%s %s", verb, ref))
	if j.targetIDs[0] == "" {
		msg := "no such container: " + ref
		j.appendLine("error: " + msg)
		j.markFailed(msg)
		return
	}
	if err := e.doContainerVerb(ctx, verb, j.targetIDs[0], j.force, j.timeout); err != nil {
		j.appendLine("error: " + err.Error())
		j.markFailed(err.Error())
		return
	}
	j.appendLine("ok")
	j.markCompleted()
	fleetTrigger() // repaint the new container state promptly
}

// runBulk fans the same verb across j.targets with bounded concurrency, streaming
// one progress line per container and an aggregate result. The job fails if ANY
// container op fails (partial-success is still surfaced line by line).
func (e *engine) runBulk(ctx context.Context, j *Job, verb string, fleetTrigger func()) {
	refs, ids := j.targets, j.targetIDs
	total := len(ids)
	if total == 0 || len(refs) != total {
		j.markFailed("no checked target containers")
		return
	}
	j.appendLine(fmt.Sprintf("bulk %s on %d container(s) (concurrency %d)", verb, total, e.bulkConcurrency))

	sem := make(chan struct{}, e.bulkConcurrency)
	var wg sync.WaitGroup
	var done, failed atomic.Int32

	for i, id := range ids {
		ref := refs[i]
		wg.Go(func() { // Go 1.25 WaitGroup.Go
			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				n := done.Add(1)
				failed.Add(1)
				j.appendLine(fmt.Sprintf("[%d/%d] %s %s: cancelled", n, total, verb, ref))
				return
			}
			var err error
			if id == "" {
				err = fmt.Errorf("no such container")
			} else {
				err = e.doContainerVerb(ctx, verb, id, j.force, j.timeout)
			}
			n := done.Add(1)
			if err != nil {
				failed.Add(1)
				j.appendLine(fmt.Sprintf("[%d/%d] %s %s: error: %v", n, total, verb, ref, err))
			} else {
				j.appendLine(fmt.Sprintf("[%d/%d] %s %s: ok", n, total, verb, ref))
			}
			fleetTrigger()
		})
	}
	wg.Wait()

	if f := int(failed.Load()); f > 0 {
		j.markFailed(fmt.Sprintf("%d of %d failed", f, total))
	} else {
		j.appendLine(fmt.Sprintf("all %d %s ok", total, verb))
		j.markCompleted()
	}
	fleetTrigger()
}

// runComposeOp executes a project-scoped compose op (up/down/pull/restart/
// recreate) via the in-process compose-v2 library. Progress + stream output are
// funneled into the job log by the per-job api.Compose (compose.go); cancellation
// flows through the job context, and composeOpTimeout bounds a wedged op.
func (e *engine) runComposeOp(ctx context.Context, j *Job, op string, fleetTrigger func()) {
	if e.compose == nil {
		j.markFailed("compose backend unavailable")
		return
	}
	name := j.snapshot().Project
	if name == "" {
		j.markFailed("no target project")
		return
	}

	// Resolve from the durable registry first; fall back to live container
	// labels so an ad-hoc / not-yet-registered running project is still operable.
	entry, ok := e.projects.get(name)
	if !ok {
		if _, live, err := e.docker.snapshot(ctx); err == nil {
			entry, ok = e.projects.resolve(name, live)
		}
	}
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

	j.appendLine(fmt.Sprintf("compose %s %s (%s)", op, name, entry.WorkingDir))
	if err := e.compose.execute(opCtx, j, op, entry, fleetTrigger); err != nil {
		if ctx.Err() != nil {
			// Cancelled (or timed out) — let the cancel path own the terminal state.
			j.appendLine("cancelled")
			j.markFailed("cancelled: " + err.Error())
		} else {
			j.appendLine("error: " + err.Error())
			j.markFailed(err.Error())
		}
		fleetTrigger()
		return
	}
	j.appendLine(fmt.Sprintf("compose %s %s ok", op, name))
	j.markCompleted()
	fleetTrigger()
}

// doContainerVerb dispatches a single verb to the docker client. force/timeout
// apply to remove (force) and stop/restart (SIGTERM grace) respectively.
func (e *engine) doContainerVerb(ctx context.Context, verb, id string, force bool, timeout *int) error {
	switch verb {
	case "start":
		return e.docker.startContainer(ctx, id)
	case "stop":
		return e.docker.stopContainer(ctx, id, timeout)
	case "restart":
		return e.docker.restartContainer(ctx, id, timeout)
	case "kill":
		return e.docker.killContainer(ctx, id)
	case "remove":
		return e.docker.removeContainer(ctx, id, force)
	default:
		return fmt.Errorf("unknown container verb %q", verb)
	}
}
