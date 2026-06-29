package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func (a *app) handleLiveness(c *gin.Context)  { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }
func (a *app) handleReadiness(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) }

// ---------------------------------------------------------------------------
// Container lifecycle (Phase 1) — every op is a SHORT async job: it returns
// 202 + job id, and the NEXT /ws/fleet snapshot confirms the new state. None are
// synchronous (stop/restart/remove-running carry a SIGTERM grace). Jobs run on a
// detached context (context.Background), like build-agent — the request context
// would cancel the op the instant the 202 is written.
// ---------------------------------------------------------------------------

// containerOpBody is the optional JSON body for a single-container op.
type containerOpBody struct {
	Force      bool   `json:"force,omitempty"`   // remove: kill a running container first
	Timeout    *int   `json:"timeout,omitempty"` // stop/restart: SIGTERM grace seconds
	TriggerKey string `json:"trigger_key,omitempty"`
}

// bulkOpBody is the JSON body for POST /v1/containers/bulk — one call fans out to
// a single bulk job over ids[], so the router proxies exactly one request.
type bulkOpBody struct {
	Action     string   `json:"action"` // start|stop|restart|kill|remove
	IDs        []string `json:"ids"`
	Force      bool     `json:"force,omitempty"`
	Timeout    *int     `json:"timeout,omitempty"`
	TriggerKey string   `json:"trigger_key,omitempty"`
}

var validBulkAction = map[string]bool{
	"start": true, "stop": true, "restart": true, "kill": true, "remove": true,
}

func (a *app) handleContainerStart(c *gin.Context)   { a.startContainerJob(c, opContainerStart) }
func (a *app) handleContainerStop(c *gin.Context)    { a.startContainerJob(c, opContainerStop) }
func (a *app) handleContainerRestart(c *gin.Context) { a.startContainerJob(c, opContainerRestart) }
func (a *app) handleContainerRemove(c *gin.Context)  { a.startContainerJob(c, opContainerRemove) }

func (a *app) startContainerJob(c *gin.Context, op string) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "container id is required"})
		return
	}
	var body containerOpBody
	_ = c.ShouldBindJSON(&body) // optional; empty body leaves zero values
	j := a.reg.start(context.Background(), JobRequest{
		Operation:  op,
		Target:     id,
		Force:      body.Force,
		Timeout:    body.Timeout,
		TriggerKey: orDefault(body.TriggerKey, "ui"),
	})
	c.JSON(http.StatusAccepted, gin.H{"job_id": j.ID, "state": j.snapshot().State})
}

func (a *app) handleContainerBulk(c *gin.Context) {
	var body bulkOpBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validBulkAction[body.Action] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be one of start|stop|restart|kill|remove"})
		return
	}
	if len(body.IDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids must be non-empty"})
		return
	}
	j := a.reg.start(context.Background(), JobRequest{
		Operation:  opContainerBulkPrefix + body.Action,
		Target:     fmt.Sprintf("%d container(s)", len(body.IDs)),
		Targets:    body.IDs,
		Force:      body.Force,
		Timeout:    body.Timeout,
		TriggerKey: orDefault(body.TriggerKey, "ui"),
	})
	c.JSON(http.StatusAccepted, gin.H{"job_id": j.ID, "state": j.snapshot().State, "count": len(body.IDs)})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (a *app) handleListJobs(c *gin.Context) {
	c.JSON(http.StatusOK, a.reg.list())
}

func (a *app) handleGetJob(c *gin.Context) {
	id := c.Param("id")
	if j, ok := a.reg.get(id); ok {
		c.JSON(http.StatusOK, j.snapshot())
		return
	}
	if jp, ok, _ := a.events.getJob(id); ok {
		c.JSON(http.StatusOK, jp)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
}

func (a *app) handleGetJobLog(c *gin.Context) {
	id := c.Param("id")
	j, ok := a.reg.get(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found or no live log"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "lines": j.logLines()})
}

func (a *app) handleCancelJob(c *gin.Context) {
	if a.reg.cancelJob(c.Param("id")) {
		c.JSON(http.StatusOK, gin.H{"cancelled": true})
		return
	}
	c.JSON(http.StatusConflict, gin.H{"error": "job not cancellable (unknown or already terminal)"})
}

// handleJobLogsWS streams a job's log: backlog then live lines, over the shared
// streamLines pump. The job registry is the SOURCE (in-process ring + channel);
// delivery is identical to container logs.
func (a *app) handleJobLogsWS(c *gin.Context) {
	conn, err := fleetUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ch, backlog, unsub, ok := a.reg.subscribe(c.Param("id"))
	if !ok {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("job not found or no live log"))
		return
	}
	defer unsub()
	streamLines(conn, backlog, ch)
}

// ---------------------------------------------------------------------------
// Image-outdated detection (Phase 3). The results normally ride the /ws/fleet
// snapshot (stamped onto containers/projects). These endpoints expose the raw
// cache for debugging + a manual refresh trigger.
// ---------------------------------------------------------------------------

// handleImageChecks returns the raw cached image-check results (debug/inspect).
func (a *app) handleImageChecks(c *gin.Context) {
	if a.images == nil {
		c.JSON(http.StatusOK, gin.H{"checks": []imageCheck{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"checks": a.images.snapshotCache()})
}

// handleImageCheckRefresh requests an out-of-band image-check pass that BYPASSES the
// TTL coalesce (debounced to one force per window), so a strategy the caller just
// changed is reloaded and recomputed within seconds instead of up to one TTL later.
// Returns 202 — the next /ws/fleet snapshot carries fresh statuses.
func (a *app) handleImageCheckRefresh(c *gin.Context) {
	if a.images == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image checker not running"})
		return
	}
	a.images.ForceRecheckDebounced()
	c.JSON(http.StatusAccepted, gin.H{"triggered": true})
}
