package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func (a *app) handleLiveness(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }

// handleReadiness reports, but never gates on, self identity: an unresolved
// identity costs only the own-container guard, and hiding the pod would take every
// other op down with it. It reads the last published view and never waits on the
// Docker daemon.
func (a *app) handleReadiness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready", "self": a.self.status()})
}

// ---------------------------------------------------------------------------
// Container lifecycle — every op is a SHORT async job: it returns 202 + job id,
// and the NEXT /ws/fleet snapshot confirms the new state. None are synchronous
// (stop/restart/remove-running carry a SIGTERM grace). Jobs run on a DETACHED
// context (context.Background) — the request context would cancel the op the
// instant the 202 is written.
// ---------------------------------------------------------------------------

// containerOpBody is the optional JSON body for a single-container op.
type containerOpBody struct {
	Force      bool   `json:"force,omitempty"`   // remove: kill a running container first
	Timeout    *int   `json:"timeout,omitempty"` // stop/restart: SIGTERM grace seconds
	TriggerKey string `json:"trigger_key,omitempty"`
}

// bulkOpBody is the JSON body for POST /v1/containers/bulk — one call fans out to
// a single bulk job over ids[], so a caller issues exactly one request.
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
	if a.refuseContainerTargets(c, op, []string{id}) {
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
	// All or nothing: a bulk request naming a protected container is refused whole,
	// so no partial fan-out runs against the rest of the selection.
	if a.refuseContainerTargets(c, opContainerBulkPrefix+body.Action, body.IDs) {
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

// containerResolveConcurrency bounds the inspects one bulk request issues.
const containerResolveConcurrency = 4

// stopsContainer reports whether a verb leaves the container not running.
func stopsContainer(op string) bool {
	switch strings.TrimPrefix(strings.TrimPrefix(op, opContainerBulkPrefix), "container.") {
	case "stop", "kill", "remove":
		return true
	}
	return false
}

// refuseContainerTargets refuses a container verb aimed at a protected container
// and returns true when it wrote a response. Every target is resolved by the
// daemon's own authority (inspect → full ID), then compared by ID: a container
// named `db` is not the agent just because the agent's ID starts with "db", and a
// renamed agent is still the agent.
//
//   - self (the agent, or a container sharing its network namespace): every verb.
//   - the control-path container: stop/kill/remove. start/restart bring it back.
//   - a target the daemon cannot find is not refused — the op fails on its own.
//   - an inspect or list failure is a 503: the request cannot be proven safe.
func (a *app) refuseContainerTargets(c *gin.Context, op string, targets []string) bool {
	_, view, proceed := a.selfForMutation(c)
	if !proceed {
		return true
	}
	if view == nil || (len(view.ids) == 0 && view.controlPath == nil) {
		return false // nothing to protect by identity (see selfForMutation)
	}
	ids, err := a.resolveContainerTargets(c.Request.Context(), targets)
	if err != nil {
		refuseSelfUnavailable(c, err, targets)
		return true
	}
	var selfT, controlT []string
	for i, t := range targets {
		switch {
		case view.isSelfID(ids[i]):
			selfT = append(selfT, t)
		case stopsContainer(op) && view.isControlPathID(ids[i]):
			controlT = append(controlT, t)
		}
	}
	if len(selfT) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("refusing %s on docker-agent's own container: the agent would stop mid-job. "+
				"The agent is managed by Ansible (docker-agent/deploy.yml).", op),
			"code":    "self_container",
			"targets": selfT,
		})
		return true
	}
	if len(controlT) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("refusing %s on the agent's control-path proxy: the agent would be left unreachable "+
				"with nothing able to start it again. restart is allowed.", op),
			"code":    "control_path_container",
			"targets": controlT,
		})
		return true
	}
	return false
}

// resolveContainerTargets inspects each target (bounded concurrency, bounded time)
// and returns full IDs aligned with targets ("" = no such container).
func (a *app) resolveContainerTargets(ctx context.Context, targets []string) ([]string, error) {
	if a.docker == nil {
		return nil, fmt.Errorf("docker client unavailable")
	}
	rctx, cancel := context.WithTimeout(ctx, selfListTimeout)
	defer cancel()
	ids := make([]string, len(targets))
	errs := make([]error, len(targets))
	sem := make(chan struct{}, containerResolveConcurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		if strings.TrimSpace(t) == "" {
			continue // names no container
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ids[i], errs[i] = a.docker.resolveContainerID(rctx, t)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("resolve container %q: %w", targets[i], err)
		}
	}
	return ids, nil
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
// Image-outdated detection. The results normally ride the /ws/fleet
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
