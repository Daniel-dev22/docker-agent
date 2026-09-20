package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func (a *app) handleLiveness(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }

// handleReadiness reports, but never gates on, self identity: an unresolved
// identity costs only the own-container guard, and hiding the pod would take every
// other op down with it. It reads the last published view and never waits on the
// Docker daemon.
func (a *app) handleReadiness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready", "self": a.self.status(),
		"projects": gin.H{
			// Registry entries that share a working directory (predating the rule that
			// refuses them): reported, not gated — the agent still serves both.
			"shared_working_dirs": a.projects.sharedWorkingDirs(),
			// Entries the index says are externally rendered whose DIRECTORY does not
			// record it (ownermark.go). Empty in a healthy agent: load writes any that
			// are missing. A name here is ownership that would not survive losing
			// projects.json — the failure the mark exists to prevent, and one that is
			// otherwise silent until the day it matters. Reported, not gated: the
			// index still protects the stack for this process's lifetime.
			"owners_not_recorded": a.projects.unmarkedOwners(),
		}})
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
	if strings.TrimSpace(id) == "" {
		refuse(c, http.StatusBadRequest, "invalid_target", "container id is required", nil)
		return
	}
	refs, ids, ok := a.checkContainerTargets(c, op, []string{id})
	if !ok {
		return
	}
	var body containerOpBody
	_ = c.ShouldBindJSON(&body) // optional; empty body leaves zero values
	j := a.reg.start(context.Background(), JobRequest{
		Operation:  op,
		Target:     refs[0],
		TargetIDs:  ids,
		Force:      body.Force,
		Timeout:    body.Timeout,
		TriggerKey: orDefault(body.TriggerKey, "ui"),
	})
	c.JSON(http.StatusAccepted, gin.H{"job_id": j.ID, "state": j.snapshot().State})
}

func (a *app) handleContainerBulk(c *gin.Context) {
	var body bulkOpBody
	if err := c.ShouldBindJSON(&body); err != nil {
		refuse(c, http.StatusBadRequest, "invalid_body", "invalid request body: "+echo(err.Error()), nil)
		return
	}
	if !validBulkAction[body.Action] {
		refuse(c, http.StatusBadRequest, "invalid_action", "action must be one of start|stop|restart|kill|remove", nil)
		return
	}
	// All or nothing: a bulk request naming a protected container is refused whole,
	// so no partial fan-out runs against the rest of the selection.
	refs, ids, ok := a.checkContainerTargets(c, opContainerBulkPrefix+body.Action, body.IDs)
	if !ok {
		return
	}
	j := a.reg.start(context.Background(), JobRequest{
		Operation:  opContainerBulkPrefix + body.Action,
		Target:     fmt.Sprintf("%d container(s)", len(ids)),
		Targets:    refs,
		TargetIDs:  ids,
		Force:      body.Force,
		Timeout:    body.Timeout,
		TriggerKey: orDefault(body.TriggerKey, "ui"),
	})
	c.JSON(http.StatusAccepted, gin.H{"job_id": j.ID, "state": j.snapshot().State, "count": len(ids)})
}

// containerResolveConcurrency bounds the inspects one bulk request issues.
const containerResolveConcurrency = 4

// maxContainerTargets caps one request's distinct targets. Every target costs one
// inspect before the job is accepted, all inside selfListTimeout (5s) at
// containerResolveConcurrency (4): 100 targets are 25 rounds, ~1.25s at 50ms per
// inspect on a Pi-class host — 4x headroom. The largest live host runs 15
// containers (kd-nas01, 2026-09-17), so the cap is ~6x the real maximum and only
// ever refuses a request that could not be one host's selection.
const maxContainerTargets = 100

// stopsContainer reports whether a verb leaves the container not running.
func stopsContainer(op string) bool {
	switch strings.TrimPrefix(strings.TrimPrefix(op, opContainerBulkPrefix), "container.") {
	case "stop", "kill", "remove":
		return true
	}
	return false
}

// checkContainerTargets decides a container verb's targets and returns the
// references (for the job log) and the full IDs the job must act on, aligned; or it
// writes the response and returns ok=false.
//
// Every target is resolved by the daemon's own authority (inspect → full ID) and
// the job then acts on that ID, never on the reference again: a reference resolved
// twice can name a different container the second time (two "db" in one request,
// the second resolving by ID prefix to the agent). Duplicate references and
// references naming the same container collapse to one.
//
//   - self (the agent, or a container sharing its network namespace): every verb.
//   - the control-path container: stop/kill/remove. start/restart bring it back.
//   - a reference the daemon cannot find acts on nothing; the job reports it.
//   - an ambiguous or malformed reference is the caller's error: 400 invalid_target.
//   - a failed list or inspect is a 503: the request cannot be proven safe.
func (a *app) checkContainerTargets(c *gin.Context, op string, targets []string) ([]string, []string, bool) {
	refs := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if strings.TrimSpace(t) == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		refs = append(refs, t)
	}
	if len(refs) == 0 {
		refuse(c, http.StatusBadRequest, "invalid_body", "ids must be non-empty", nil)
		return nil, nil, false
	}
	if len(refs) > maxContainerTargets {
		refuse(c, http.StatusBadRequest, "too_many_targets",
			fmt.Sprintf("%d distinct targets exceeds the limit of %d per request; split the selection",
				len(refs), maxContainerTargets), nil)
		return nil, nil, false
	}
	// A container name is at most 255 bytes and an ID 64: anything longer names
	// nothing, and must not reach the daemon or an echo.
	var tooLong []string
	for _, r := range refs {
		if len(r) > maxProjectNameLen {
			tooLong = append(tooLong, r)
		}
	}
	if len(tooLong) > 0 {
		refuse(c, http.StatusBadRequest, "invalid_target",
			fmt.Sprintf("container reference longer than %d bytes", maxProjectNameLen), gin.H{"targets": tooLong})
		return nil, nil, false
	}
	_, view, proceed := a.selfForMutation(c)
	if !proceed {
		return nil, nil, false
	}
	resolved, invalid, err := a.resolveContainerTargets(c.Request.Context(), refs)
	if len(invalid) > 0 {
		refuse(c, http.StatusBadRequest, "invalid_target",
			"ambiguous or malformed container reference; use a full ID or an exact name", gin.H{"targets": invalid})
		return nil, nil, false
	}
	if err != nil {
		refuseSelfUnavailable(c, err, refs)
		return nil, nil, false
	}
	outRefs := make([]string, 0, len(refs))
	ids := make([]string, 0, len(refs))
	byID := make(map[string]struct{}, len(refs))
	var selfT, controlT []string
	for i, ref := range refs {
		id := resolved[i]
		if id != "" {
			if _, dup := byID[id]; dup {
				continue
			}
			byID[id] = struct{}{}
		}
		switch {
		case view.isSelfID(id):
			selfT = append(selfT, ref)
		case stopsContainer(op) && view.isControlPathID(id):
			controlT = append(controlT, ref)
		}
		outRefs = append(outRefs, ref)
		ids = append(ids, id)
	}
	if len(selfT) > 0 {
		refuse(c, http.StatusConflict, "self_container",
			fmt.Sprintf("refusing %s on docker-agent's own container: the agent would stop mid-job. "+
				"The agent is managed by Ansible (docker-agent/deploy.yml).", op), gin.H{"targets": selfT})
		return nil, nil, false
	}
	if len(controlT) > 0 {
		refuse(c, http.StatusConflict, "control_path_container",
			fmt.Sprintf("refusing %s on the agent's control-path proxy: the agent would be left unreachable "+
				"with nothing able to start it again. start and restart are allowed.", op), gin.H{"targets": controlT})
		return nil, nil, false
	}
	return outRefs, ids, true
}

// resolveContainerTargets inspects each reference (at most
// containerResolveConcurrency at once, all inside selfListTimeout) and returns full
// IDs aligned with refs ("" = no such container), the references the daemon called
// ambiguous or malformed, and the first other failure. Every target's outcome
// counts: one failed inspect anywhere fails the whole request.
func (a *app) resolveContainerTargets(ctx context.Context, refs []string) ([]string, []string, error) {
	if a.docker == nil {
		return nil, nil, fmt.Errorf("docker client unavailable")
	}
	rctx, cancel := context.WithTimeout(ctx, selfListTimeout)
	defer cancel()
	ids := make([]string, len(refs))
	errs := make([]error, len(refs))
	sem := make(chan struct{}, containerResolveConcurrency)
	var wg sync.WaitGroup
spawn:
	for i, ref := range refs {
		// Take the slot BEFORE starting the goroutine, so a large request never holds
		// more than containerResolveConcurrency goroutines; stop starting work once
		// the deadline has passed.
		select {
		case sem <- struct{}{}:
		case <-rctx.Done():
			for k := i; k < len(refs); k++ {
				errs[k] = rctx.Err()
			}
			break spawn
		}
		wg.Go(func() {
			defer func() { <-sem }()
			ids[i], errs[i] = a.docker.resolveContainerID(rctx, ref)
		})
	}
	wg.Wait()
	var invalid []string
	var firstErr error
	for i, err := range errs {
		switch {
		case err == nil:
		case cerrdefs.IsInvalidArgument(err):
			invalid = append(invalid, refs[i])
		case firstErr == nil:
			firstErr = fmt.Errorf("resolve container %q: %w", echo(refs[i]), err)
		}
	}
	return ids, invalid, firstErr
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
	refuse(c, http.StatusConflict, "job_not_cancellable", "job not cancellable (unknown or already terminal)", nil)
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
