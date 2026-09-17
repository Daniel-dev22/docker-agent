package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Daniel-dev22/agent-kit-go/jobstore"
	"github.com/Daniel-dev22/agent-kit-go/reconcile"
	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
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
	// self is this agent's own containers + compose projects, which no endpoint
	// may act on, and its control-path container (selfid.go).
	self *selfIdentity
}

func newApp(ctx context.Context, cfg Config) (*app, error) {
	cc := buildControlCenterClient(cfg)
	dc, err := newDockerClient(cfg.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	self := newSelfIdentity(mountinfoPath, cfg.TraefikDockerDNS)
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

	a := &app{cfg: cfg, cc: cc, docker: dc, compose: cb, projects: projects, events: events, reg: reg, self: self}
	a.images = newImageChecker(cfg, dc, cc)
	eng.setImageChecker(a.images)               // the update engine reuses strategy + clients
	a.images.setProjectPlanner(eng.planProject) // coupled-project status = the update's dry-run (DRY)
	a.fleet = newFleetHub(a)
	reg.setFleet(a.fleet)
	// Discovery feed (full-snapshot push to the controller). Wired after the fleet
	// hub so it can reuse SnapshotNow; the image checker triggers it after each
	// pass so newly-detected outdated images surface promptly.
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
	go a.images.Run(ctx)         // slow jittered image-outdated pass
	go a.discovery.Run(ctx)      // periodic discovery snapshot feed
}

// enrichProjectsOnce reads the current container set once at boot and
// auto-adopts any running compose project not already in projects.json, so an
// operator never has to register a project that's already up. Stopped projects
// already in the registry are untouched.
func (a *app) enrichProjectsOnce(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	summaries, err := a.docker.listContainers(sctx)
	if err != nil {
		a.self.observeFailed(err)
		slog.Warn("project enrichment skipped — snapshot failed", "error", err)
		return
	}
	// Publish self identity from this first list so readiness shows it before any
	// dashboard has asked for a snapshot.
	a.self.observe(summaries)
	a.projects.enrichFromLive(groupComposeProjects(summaries))
}

// selfListTimeout bounds the container list a mutating request takes to learn what
// it must not touch. It is derived from the request context, never held under a
// lock, and a failure is a 503 the caller can retry.
const selfListTimeout = 5 * time.Second

// observeNow takes a fresh bounded container list and republishes self identity
// from it.
func (a *app) observeNow(ctx context.Context) ([]container.Summary, *selfView, error) {
	if a.docker == nil {
		return nil, nil, errors.New("docker client unavailable")
	}
	lctx, cancel := context.WithTimeout(ctx, selfListTimeout)
	defer cancel()
	summaries, err := a.docker.listContainers(lctx)
	if err != nil {
		a.self.observeFailed(err)
		return nil, nil, err
	}
	return summaries, a.self.observe(summaries), nil
}

// selfForMutation is the gate every mutating handler passes first. It returns the
// fresh container list and self view, or writes a 503 and returns ok=false when the
// agent knows which container it is but cannot see the current list — allowing
// the request then would be allowing it blind.
//
// When mountinfo named no container (not running under Docker), there is nothing
// to protect by identity: a list failure is reported in readiness, and the request
// proceeds with whatever the list could not tell it (summaries and view nil).
func (a *app) selfForMutation(c *gin.Context) ([]container.Summary, *selfView, bool) {
	summaries, view, err := a.observeNow(c.Request.Context())
	if err != nil {
		if a.self.known() {
			refuseSelfUnavailable(c, err, nil)
			return nil, nil, false
		}
		return nil, nil, true
	}
	return summaries, view, true
}

// startReconcile POSTs this agent's authoritative job set to the controller on
// boot + every 5m, so a lost terminal event can't strand a row "running". An
// agent that has run no jobs posts an empty snapshot — a correct no-op.
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
