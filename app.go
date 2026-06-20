package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Daniel-dev22/agent-kit-go/jobstore"
	"github.com/Daniel-dev22/agent-kit-go/reconcile"
)

// app wires the docker-agent's subsystems: the pooled controller client, the
// docker SDK client, the events buffer (sqlite + durable outbox), the job
// registry (operation lifecycle + engine), and the fleet hub (live container
// snapshots to the dashboard).
type app struct {
	cfg    Config
	cc     *http.Client
	docker *dockerClient
	events *eventBuffer
	reg    *jobRegistry
	fleet  *fleetHub
}

func newApp(_ context.Context, cfg Config) (*app, error) {
	cc := buildControlCenterClient(cfg)
	dc, err := newDockerClient(cfg.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	events, err := newEventBuffer(cfg, cc)
	if err != nil {
		return nil, fmt.Errorf("event buffer: %w", err)
	}
	eng := newEngine(cfg, dc)
	reg := newJobRegistry(cfg, eng, events)
	reg.setHook(events.handleJobEvent)

	a := &app{cfg: cfg, cc: cc, docker: dc, events: events, reg: reg}
	a.fleet = newFleetHub(a)
	reg.setFleet(a.fleet)
	return a, nil
}

func (a *app) close() {
	if a.events != nil {
		a.events.close()
	}
	if a.docker != nil {
		_ = a.docker.close()
	}
}

func (a *app) startBackgroundWorkers(ctx context.Context) {
	a.events.Start(ctx) // outbox drain
	go a.fleet.Run(ctx) // fleet broadcaster
	go a.startReconcile(ctx)
}

// startReconcile POSTs this agent's authoritative job set to the controller on
// boot + every 5m, so a lost terminal event can't strand a row "running". In
// Phase 0 (no producers) the snapshot is empty — a correct no-op that exercises
// the path end to end.
func (a *app) startReconcile(ctx context.Context) {
	reconcile.Run(ctx, reconcile.Config{
		Client:   a.cc,
		URL:      a.cfg.ControlCenterURL + "/api/docker/reconcile",
		Site:     a.cfg.SiteID,
		Node:     a.cfg.NodeName,
		Interval: 5 * time.Minute,
		Snapshot: func(sctx context.Context) ([]jobstore.JobRef, error) {
			return jobstore.ListForReconcile(sctx, a.events.DB(), jobstore.ListOptions{
				Table:                   "compose_jobs",
				ExitCodeColumn:          "exit_code",
				RecentTerminalPredicate: "completed_at_ns >= ?",
				RecentTerminalArg:       time.Now().Add(-24 * time.Hour).UnixNano(),
			})
		},
	})
}
