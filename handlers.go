package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func (a *app) handleLiveness(c *gin.Context)  { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }
func (a *app) handleReadiness(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ready"}) }

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

// handleJobLogsWS streams a job's log: backlog then live lines.
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
	// Frontend JobLogStream expects raw newline-delimited text frames.
	writeLine := func(line string) bool {
		return conn.WriteMessage(websocket.TextMessage, []byte(line+"\n")) == nil
	}
	for _, line := range backlog {
		if !writeLine(line) {
			return
		}
	}
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case line, open := <-ch:
			if !open {
				return
			}
			if !writeLine(line) {
				return
			}
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		}
	}
}
