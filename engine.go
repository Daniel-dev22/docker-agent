package main

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// Operation names. Container-scoped lifecycle ops (Phase 1) act on a single
// Target; the bulk variants ("container.bulk.<verb>") fan out over Targets.
// Compose ops (Phase 2) and the update engine (Phase 3.5) add more.
const (
	opContainerStart      = "container.start"
	opContainerStop       = "container.stop"
	opContainerRestart    = "container.restart"
	opContainerRemove     = "container.remove"
	opContainerKill       = "container.kill"
	opContainerBulkPrefix = "container.bulk." // + verb (start|stop|restart|kill|remove)
)

// engine executes a Job's operation against the docker engine, streaming output
// to the job's log ring (j.appendLine) and emitting lifecycle events via
// onChange. It holds the SDK client + compose registry needed by later phases.
//
// Operation handlers are added per phase:
//   - Phase 1: container lifecycle (start/stop/restart/remove/kill, bulk).
//   - Phase 2: compose ops (up/down/pull/restart/recreate) via the compose-v2
//     Go library, with progress events fed into j.appendLine.
//   - Phase 3.5: the stack-update engine (resolve→snapshot→pull+up→health-wait→
//     rollback).
type engine struct {
	cfg    Config
	docker *dockerClient

	// bulkConcurrency caps simultaneous container ops in a bulk fan-out (the
	// duplicacy DUPLICACY_MAX_CONCURRENT_* posture — keep the Pi from thrashing).
	bulkConcurrency int
}

func newEngine(cfg Config, dc *dockerClient) *engine {
	bc := getEnvInt("DOCKER_BULK_CONCURRENCY", defaultBulkConcurrency())
	if bc < 1 {
		bc = 1
	}
	return &engine{cfg: cfg, docker: dc, bulkConcurrency: bc}
}

// defaultBulkConcurrency derives from the (cgroup-accurate, Go 1.25) GOMAXPROCS
// and is clamped to [2,4] so a NAS doesn't fan out wildly and a Pi stays gentle.
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
	// Phase 2/3.5 add compose ops (up/down/pull/restart, update) here.
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

// runContainerOp executes a single-container lifecycle verb on j.Target.
func (e *engine) runContainerOp(ctx context.Context, j *Job, verb string, fleetTrigger func()) {
	id := j.snapshot().Target
	if id == "" {
		j.markFailed("no target container")
		return
	}
	j.appendLine(fmt.Sprintf("%s %s", verb, id))
	if err := e.doContainerVerb(ctx, verb, id, j.force, j.timeout); err != nil {
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
	ids := j.targets
	total := len(ids)
	if total == 0 {
		j.markFailed("no target containers")
		return
	}
	j.appendLine(fmt.Sprintf("bulk %s on %d container(s) (concurrency %d)", verb, total, e.bulkConcurrency))

	sem := make(chan struct{}, e.bulkConcurrency)
	var wg sync.WaitGroup
	var done, failed atomic.Int32

	for _, id := range ids {
		wg.Go(func() { // Go 1.25 WaitGroup.Go
			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				n := done.Add(1)
				failed.Add(1)
				j.appendLine(fmt.Sprintf("[%d/%d] %s %s: cancelled", n, total, verb, id))
				return
			}
			err := e.doContainerVerb(ctx, verb, id, j.force, j.timeout)
			n := done.Add(1)
			if err != nil {
				failed.Add(1)
				j.appendLine(fmt.Sprintf("[%d/%d] %s %s: error: %v", n, total, verb, id, err))
			} else {
				j.appendLine(fmt.Sprintf("[%d/%d] %s %s: ok", n, total, verb, id))
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
