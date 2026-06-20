package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Daniel-dev22/agent-kit-go/jobstore"
)

// sweepOrphanJobs runs at boot, BEFORE the HTTP listener: any job left
// running/pending by a prior container exit is flipped to failed in the local
// compose_jobs table, and a synthetic terminal event is enqueued via the durable
// outbox so the controller converges when reachable (no zombie "running").
func sweepOrphanJobs(ctx context.Context, cfg Config, events *eventBuffer) {
	ids, err := jobstore.MarkOrphanedFailed(ctx, events.DB(), jobstore.SweepOptions{
		Table:          "compose_jobs",
		ErrorColumn:    "error",
		ErrorMessage:   "agent restart orphan (boot sweep): operation interrupted by container exit",
		ExitCodeColumn: "exit_code",
		ExitCode:       137,
	})
	if err != nil {
		slog.Warn("orphan sweep failed", "error", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	slog.Warn("orphan-swept interrupted jobs", "count", len(ids))
	now := time.Now().UTC()
	for _, id := range ids {
		jp, ok, _ := events.getJob(id)
		if !ok {
			continue
		}
		payload := EventPayload{
			JobID:       id,
			Site:        cfg.SiteID,
			Node:        cfg.NodeName,
			Project:     jp.Project,
			Operation:   jp.Operation,
			Target:      jp.Target,
			State:       JobFailed,
			Event:       EventFailed,
			CompletedAt: &now,
			ExitCode:    137,
			ErrorMsg:    "agent restart orphan (boot sweep)",
			TriggerKey:  jp.TriggerKey,
			EmittedAt:   now,
		}
		body, _ := json.Marshal(payload)
		events.outbox.Enqueue(id, string(EventFailed), body)
	}
}
