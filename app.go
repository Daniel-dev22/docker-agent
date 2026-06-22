package main

import (
	"context"
	"fmt"
	"log/slog"
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
	cfg       Config
	cc        *http.Client
	docker    *dockerClient
	compose   *composeBackend
	projects  *composeRegistry
	events    *eventBuffer
	reg       *jobRegistry
	fleet     *fleetHub
	images    *imageChecker
	discovery *discoveryPusher
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

	// Durable compose-project registry (projects.json). Missing file = empty
	// registry; a malformed file is fatal (surface corruption, don't silently
	// drop the index).
	projects := newComposeRegistry(cfg.ComposeRegistryPath, cfg.ComposeRoot)
	if err := projects.load(); err != nil {
		return nil, fmt.Errorf("load compose registry: %w", err)
	}

	// In-process compose-v2 backend. A failure here (e.g. docker cli init) must
	// NOT kill the agent — read-only fleet + container lifecycle still work, and
	// compose ops fail loudly per-op. Log and continue with a nil backend.
	cb, err := newComposeBackend(cfg)
	if err != nil {
		slog.Error("compose backend init failed — compose ops disabled", "error", err)
		cb = nil
	}

	eng := newEngine(cfg, dc, cb, projects)
	reg := newJobRegistry(cfg, eng, events)
	reg.setHook(events.handleJobEvent)

	a := &app{cfg: cfg, cc: cc, docker: dc, compose: cb, projects: projects, events: events, reg: reg}
	a.images = newImageChecker(cfg, dc, cc)
	eng.setImageChecker(a.images)               // Phase 3.5: update engine reuses strategy + clients
	a.images.setProjectPlanner(eng.planProject) // coupled-project status = the update's dry-run (DRY)
	a.fleet = newFleetHub(a)
	reg.setFleet(a.fleet)
	// Phase 4: discovery feed (full-snapshot push to controller). Wired after
	// the fleet hub so it can reuse SnapshotNow; the image checker triggers it
	// after each pass so newly-detected outdated images surface promptly.
	a.discovery = newDiscoveryPusher(a)
	a.images.setAfterPass(a.discovery.Trigger)
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
	a.events.Start(ctx)          // outbox drain
	go a.fleet.Run(ctx)          // fleet broadcaster
	go a.startReconcile(ctx)     //
	go a.enrichProjectsOnce(ctx) // auto-adopt running compose projects into the registry
	go a.images.Run(ctx)         // slow jittered image-outdated pass (Phase 3)
	go a.discovery.Run(ctx)      // periodic discovery-table feed (Phase 4)
}

// enrichProjectsOnce reads the current container set once at boot and
// auto-adopts any running compose project not already in projects.json, so an
// operator never has to register a project that's already up. Stopped projects
// already in the registry are untouched.
func (a *app) enrichProjectsOnce(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, live, err := a.docker.snapshot(sctx)
	if err != nil {
		slog.Warn("project enrichment skipped — snapshot failed", "error", err)
		return
	}
	a.projects.enrichFromLive(live)
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
