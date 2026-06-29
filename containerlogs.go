package main

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// defaultLogTail is the initial backlog Docker replays inline before the live
// follow (and the default history-window size).
const defaultLogTail = "500"

// handleContainerLogsWS streams a container's live logs over the shared
// streamLines pump: Docker replays the Tail backlog inline, then follows. Lines
// carry RFC3339 timestamps so the client can page further back via
// handleContainerLogsHistory. Only the SOURCE (a demuxed Docker stream) is
// container-specific; delivery is identical to job logs.
//
// The Docker stream is opened BEFORE the WS upgrade so a missing container
// surfaces as a clean 404 JSON (you can't send a typed error after upgrade, and
// a raw 500 would be relayed verbatim across the mTLS tunnel for remote nodes).
// It runs on a detached context whose lifetime is the stop() func — the request
// context can be cancelled the instant the handler hijacks the connection.
func (a *app) handleContainerLogsWS(c *gin.Context) {
	q := logQuery{
		Tail:       c.DefaultQuery("tail", defaultLogTail),
		Since:      c.Query("since"),
		Timestamps: true,
	}
	ch, stop, err := a.docker.containerLogLines(context.Background(), c.Param("id"), q)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "container logs unavailable: " + err.Error()})
		return
	}
	defer stop()

	conn, err := fleetUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	streamLines(conn, nil, ch)
}

// handleContainerLogsHistory returns a bounded, non-following window of a
// container's logs for the viewer's back-paging: ?until=<oldest-ts> walks
// backwards, ?since/?tail bound the window. Degrades to 200 + empty lines (+ an
// error field) rather than a raw 500 so the cross-site proxy never relays a scary
// error and the viewer simply stops paging.
func (a *app) handleContainerLogsHistory(c *gin.Context) {
	q := logQuery{
		Tail:       c.DefaultQuery("tail", defaultLogTail),
		Since:      c.Query("since"),
		Until:      c.Query("until"),
		Timestamps: true,
	}
	lines, err := a.docker.containerLogsHistory(c.Request.Context(), c.Param("id"), q)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"lines": []string{}, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"lines": lines})
}
