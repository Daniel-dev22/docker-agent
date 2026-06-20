package main

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	kitfleet "github.com/Daniel-dev22/agent-kit-go/fleet"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type hostInfo struct {
	Name          string `json:"name"`
	Site          string `json:"site"`
	NumCPU        int    `json:"num_cpu"`
	DockerVersion string `json:"docker_version,omitempty"`
}

// DockerSnapshot is what the dashboard renders per host: host facts + the live
// container list + the compose-project grouping.
type DockerSnapshot struct {
	Host            hostInfo          `json:"host"`
	Containers      []ContainerStatus `json:"containers"`
	ComposeProjects []ComposeProject  `json:"compose_projects"`
}

type fleetEnvelope struct {
	Type string         `json:"type"` // "snapshot"
	Data DockerSnapshot `json:"data"`
}

type fleetHub struct {
	hub *kitfleet.Hub[fleetEnvelope]
}

func newFleetHub(a *app) *fleetHub {
	build := func(ctx context.Context) (fleetEnvelope, error) {
		// runtime.NumCPU is cgroup-accurate on Go 1.25+, so it reflects the Pi's
		// real CPU quota. The container list is one bounded Engine API call.
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		containers, projects, err := a.docker.snapshot(cctx)
		if err != nil {
			return fleetEnvelope{}, err
		}
		// Merge stopped-but-registered projects so a managed stack appears even
		// when all its containers are down, and flag running ones as Managed.
		if a.projects != nil {
			projects = a.projects.mergeKnown(projects)
		}
		return fleetEnvelope{
			Type: "snapshot",
			Data: DockerSnapshot{
				Host: hostInfo{
					Name:          a.cfg.NodeName,
					Site:          a.cfg.SiteID,
					NumCPU:        runtime.NumCPU(),
					DockerVersion: a.docker.serverVersion,
				},
				Containers:      containers,
				ComposeProjects: projects,
			},
		}, nil
	}
	return &fleetHub{hub: kitfleet.NewHub(build, kitfleet.DefaultLivenessTick)}
}

func (f *fleetHub) Run(ctx context.Context) { f.hub.Run(ctx) }
func (f *fleetHub) Trigger()                { f.hub.Trigger() }

var fleetUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// handleFleetWS streams a greeting snapshot then live coalesced snapshots.
func (a *app) handleFleetWS(c *gin.Context) {
	conn, err := fleetUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ch, unsub := a.fleet.hub.Subscribe()
	defer unsub()

	if snap, err := a.fleet.hub.SnapshotNow(c.Request.Context()); err == nil {
		_ = conn.WriteJSON(snap)
	} else {
		slog.Warn("fleet snapshot failed", "error", err)
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	// drain client control frames so close is detected
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()
	for {
		select {
		case snap, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.WriteJSON(snap); err != nil {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}
}
