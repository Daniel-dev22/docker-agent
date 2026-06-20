package main

import (
	"context"
	"log/slog"
)

// engine executes a Job's operation against the docker engine, streaming output
// to the job's log ring (j.appendLine) and emitting lifecycle events via
// onChange. It holds the SDK client + compose registry needed by later phases.
//
// Operation handlers are added per phase:
//   - Phase 1: container lifecycle (start/stop/restart/remove, bulk).
//   - Phase 2: compose ops (up/down/pull/restart/recreate) via the compose-v2
//     Go library, with progress events fed into j.appendLine.
//   - Phase 3.5: the stack-update engine (resolve→snapshot→pull+up→health-wait→
//     rollback).
//
// Phase 0 ships no producers, so the dispatch has no cases yet; run() is wired
// (jobRegistry.start calls it) but no HTTP route triggers a job.
type engine struct {
	cfg    Config
	docker *dockerClient
}

func newEngine(cfg Config, dc *dockerClient) *engine {
	return &engine{cfg: cfg, docker: dc}
}

// run drives one job to a terminal state, emitting started → (completed|failed|
// cancelled). fleetTrigger is a cheap fleet-only nudge for live progress between
// lifecycle events (used by long compose ops in Phase 2).
func (e *engine) run(ctx context.Context, j *Job, onChange func(JobEvent), fleetTrigger func()) {
	j.setRunning()
	onChange(EventStarted)

	op := j.snapshot().Operation
	switch op {
	// Phase 1/2/3.5 add cases here (container.* , up/down/pull/restart, update).
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
