package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	eventoutbox "github.com/Daniel-dev22/agent-kit-go/eventoutbox"
	_ "modernc.org/sqlite" // pure-Go sqlite driver, works with CGO_ENABLED=0
)

// EventPayload is the body POSTed to the controller on every job lifecycle
// transition. The controller persists the same shape as its job / job-event
// history.
type EventPayload struct {
	JobID       string     `json:"job_id"`
	Site        string     `json:"site"`
	Node        string     `json:"node"`
	Project     string     `json:"project,omitempty"`
	Operation   string     `json:"operation"`
	Target      string     `json:"target,omitempty"`
	State       JobState   `json:"state"`
	Event       JobEvent   `json:"event"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ExitCode    int        `json:"exit_code"`
	ErrorMsg    string     `json:"error,omitempty"`
	LineCount   int        `json:"line_count"`
	TriggerKey  string     `json:"trigger_key,omitempty"`
	Services    []string   `json:"services,omitempty"`
	EmittedAt   time.Time  `json:"emitted_at"`
}

// eventBuffer owns the agent's events.sqlite: the durable outbound queue (kit
// eventoutbox, owns pending_events) plus a local compose_jobs table that gives
// the fleet snapshot restart-survival (the in-memory registry starts empty after
// a restart; this retains recent history for the dashboard + the orphan sweep).
// sqliteBusyTimeout is how long a statement on events.sqlite waits for another
// connection's lock before failing. The driver does not abandon that wait when a
// context expires, so a write that must be bounded lowers it (onBoundedConn).
const sqliteBusyTimeout = 5 * time.Second

type eventBuffer struct {
	cfg    Config
	db     *sql.DB
	outbox *eventoutbox.Outbox
}

func newEventBuffer(cfg Config, client *http.Client) (*eventBuffer, error) {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir config dir: %w", err)
	}
	dbPath := filepath.Join(cfg.ConfigDir, "events.sqlite")
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)", dbPath, sqliteBusyTimeout.Milliseconds()))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS compose_jobs (
			id              TEXT PRIMARY KEY,
			project         TEXT,
			operation       TEXT,
			target          TEXT,
			state           TEXT,
			started_at_ns   INTEGER,
			completed_at_ns INTEGER,
			exit_code       INTEGER,
			error           TEXT,
			line_count      INTEGER,
			trigger_key     TEXT,
			updated_at_ns   INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_compose_jobs_updated ON compose_jobs (updated_at_ns);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	urlFor := func(jobID string) string {
		return cfg.ControlCenterURL + "/api/docker/jobs/" + jobID + "/event"
	}
	outbox, err := eventoutbox.New(db, client, urlFor)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("event outbox: %w", err)
	}
	return &eventBuffer{cfg: cfg, db: db, outbox: outbox}, nil
}

func (e *eventBuffer) DB() *sql.DB {
	if e == nil {
		return nil
	}
	return e.db
}

func (e *eventBuffer) Start(ctx context.Context) {
	if e != nil {
		e.outbox.Start(ctx)
	}
}

func (e *eventBuffer) close() {
	if e == nil {
		return
	}
	e.outbox.Close()
	if e.db != nil {
		_ = e.db.Close()
	}
}

// handleJobEvent is the JobEventHook: persist to the local compose_jobs table
// (restart-survival) and enqueue for durable delivery to the controller.
func (e *eventBuffer) handleJobEvent(j *Job, evt JobEvent) {
	snap := j.snapshot()
	body, err := e.payloadFor(snap, evt)
	if err != nil {
		slog.Error("marshal event payload failed", "error", err, "job", snap.ID)
		return
	}
	e.upsertJob(snap)
	e.outbox.Enqueue(snap.ID, string(evt), body)
}

// payloadFor is the event the controller ingests for one job state.
func (e *eventBuffer) payloadFor(snap jobPublic, evt JobEvent) ([]byte, error) {
	return json.Marshal(EventPayload{
		JobID:       snap.ID,
		Site:        e.cfg.SiteID,
		Node:        e.cfg.NodeName,
		Project:     snap.Project,
		Operation:   snap.Operation,
		Target:      snap.Target,
		State:       snap.State,
		Event:       evt,
		StartedAt:   tPtr(snap.StartedAt),
		CompletedAt: tPtr(snap.CompletedAt),
		ExitCode:    snap.ExitCode,
		ErrorMsg:    snap.ErrorMsg,
		LineCount:   snap.LineCount,
		TriggerKey:  snap.TriggerKey,
		Services:    snap.Services,
		EmittedAt:   time.Now().UTC(),
	})
}

func tsToNs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func nsToTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func (e *eventBuffer) upsertJob(snap jobPublic) {
	if e == nil || e.db == nil {
		return
	}
	_, err := e.db.Exec(`
		INSERT INTO compose_jobs (id, project, operation, target, state,
			started_at_ns, completed_at_ns, exit_code, error, line_count, trigger_key, updated_at_ns)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			project=excluded.project, operation=excluded.operation, target=excluded.target,
			state=excluded.state, started_at_ns=excluded.started_at_ns,
			completed_at_ns=excluded.completed_at_ns, exit_code=excluded.exit_code,
			error=excluded.error, line_count=excluded.line_count, trigger_key=excluded.trigger_key,
			updated_at_ns=excluded.updated_at_ns`,
		snap.ID, snap.Project, snap.Operation, snap.Target, string(snap.State),
		tsToNs(snap.StartedAt), tsToNs(snap.CompletedAt),
		snap.ExitCode, snap.ErrorMsg, snap.LineCount, snap.TriggerKey, time.Now().UnixNano(),
	)
	if err != nil {
		slog.Warn("persist job failed", "error", err, "job", snap.ID)
	}
}

const jobSelectCols = `id, project, operation, target, state,
	started_at_ns, completed_at_ns, exit_code, error, line_count, trigger_key`

func scanJob(s interface{ Scan(...any) error }) (jobPublic, error) {
	var (
		jp                     jobPublic
		state                  string
		startedNs, completedNs int64
	)
	if err := s.Scan(&jp.ID, &jp.Project, &jp.Operation, &jp.Target, &state,
		&startedNs, &completedNs, &jp.ExitCode, &jp.ErrorMsg, &jp.LineCount, &jp.TriggerKey); err != nil {
		return jobPublic{}, err
	}
	jp.State = JobState(state)
	jp.StartedAt = nsToTime(startedNs)
	jp.CompletedAt = nsToTime(completedNs)
	return jp, nil
}

func (e *eventBuffer) listRecentJobs(limit int) ([]jobPublic, error) {
	if e == nil || e.db == nil {
		return nil, nil
	}
	rows, err := e.db.Query(`SELECT `+jobSelectCols+` FROM compose_jobs ORDER BY updated_at_ns DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []jobPublic
	for rows.Next() {
		jp, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, jp)
	}
	return out, rows.Err()
}

func (e *eventBuffer) getJob(id string) (jobPublic, bool, error) {
	if e == nil || e.db == nil {
		return jobPublic{}, false, nil
	}
	jp, err := scanJob(e.db.QueryRow(`SELECT `+jobSelectCols+` FROM compose_jobs WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return jobPublic{}, false, nil
	}
	if err != nil {
		return jobPublic{}, false, err
	}
	return jp, true, nil
}
