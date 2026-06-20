package main

import (
	"context"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Job model — one async operation against the docker engine.
//
// Unlike build-agent (whose Build fans out to N services with buildx sub-steps),
// a docker-agent Job is a single operation: a container lifecycle op (Phase 1)
// or a compose op / stack update (Phase 2+). Operation names the verb
// ("up"|"down"|"pull"|"restart"|"recreate"|"update"|"container.start"|
// "container.stop"|"container.restart"|"container.remove"|"bulk"); Project is the
// compose project for project-scoped ops; Target is the container id/name for
// container-scoped ops. Every mutation is async + tracked (nothing is "fast").
//
// Phase 0 ships NO producers (read-only fleet only) — the registry, engine
// dispatch, and durable event/reconcile plumbing are present and ready so later
// phases add only the operation handlers + their HTTP routes.
// ---------------------------------------------------------------------------

type JobState string

const (
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobCompleted JobState = "completed"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

// JobEvent mirrors the kit/controller lifecycle vocabulary.
type JobEvent string

const (
	EventStarted   JobEvent = "started"
	EventProgress  JobEvent = "progress"
	EventCompleted JobEvent = "completed"
	EventFailed    JobEvent = "failed"
	EventCancelled JobEvent = "cancelled"
)

// JobEventHook is registered by events.go to receive lifecycle notifications.
type JobEventHook func(j *Job, evt JobEvent)

const (
	ringBufferSize     = 2000 // log lines kept in memory per job
	recentJobsRetained = 50
	subscriberBuffer   = 256
)

// JobRequest is the wire body that starts a job. Phase 1 adds the container
// lifecycle routes; Phase 2 the compose routes.
//
// Target is the single container id for a container-scoped op; Targets carries
// the id list for a bulk op (Operation "container.bulk.<verb>"). Force/Timeout
// are op options (remove force, stop/restart SIGTERM grace). Targets/Force/
// Timeout are NOT persisted to the controller — they live only on the in-memory
// Job for the duration of the run.
type JobRequest struct {
	Operation  string   `json:"operation"`
	Project    string   `json:"project,omitempty"`
	Target     string   `json:"target,omitempty"`
	Targets    []string `json:"targets,omitempty"`
	Force      bool     `json:"force,omitempty"`
	Timeout    *int     `json:"timeout,omitempty"`
	TriggerKey string   `json:"trigger_key,omitempty"`
	// Stack-update (Phase 3.5) options. OverrideImage deploys an exact image (the
	// traefik/manual path); OverrideService names the target service when a
	// multi-service project's override target can't be inferred. In-memory only.
	OverrideImage   string `json:"override_image,omitempty"`
	OverrideService string `json:"override_service,omitempty"`
}

// jobPublic carries the JSON-serializable fields of a Job, split out so
// snapshot() can return a lock-free copy without copying the mutex.
type jobPublic struct {
	ID          string    `json:"id"`
	Project     string    `json:"project,omitempty"`
	Operation   string    `json:"operation"`
	Target      string    `json:"target,omitempty"`
	State       JobState  `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	ExitCode    int       `json:"exit_code"`
	ErrorMsg    string    `json:"error,omitempty"`
	LineCount   int       `json:"line_count"`
	TriggerKey  string    `json:"trigger_key,omitempty"`
}

// Job is one operation tracked through its lifecycle, with a bounded log ring
// and live subscribers (the /ws/jobs/:id/logs stream).
//
// targets/force/timeout are the in-memory op parameters (not serialized): set
// once at construction in jobRegistry.start (before the run goroutine launches,
// so they're safe to read without a lock during run).
type Job struct {
	jobPublic

	targets []string
	force   bool
	timeout *int
	// Stack-update (Phase 3.5) op params — in-memory, set at construction.
	overrideImage   string
	overrideService string

	mu          sync.Mutex
	cancel      context.CancelFunc
	ringBuffer  []string
	subscribers map[chan string]struct{}
}

func (j *Job) snapshot() jobPublic {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jobPublic
}

// setRunning marks the job running and stamps the start time.
func (j *Job) setRunning() {
	j.mu.Lock()
	j.State = JobRunning
	j.StartedAt = time.Now().UTC()
	j.mu.Unlock()
}

// markCompleted sets the terminal success state (unless cancelled).
func (j *Job) markCompleted() {
	j.mu.Lock()
	if j.State != JobCancelled {
		j.State = JobCompleted
		j.ExitCode = 0
		j.CompletedAt = time.Now().UTC()
	}
	j.mu.Unlock()
}

// markFailed sets the terminal failure state (unless cancelled).
func (j *Job) markFailed(msg string) {
	j.mu.Lock()
	if j.State != JobCancelled {
		j.State = JobFailed
		j.ExitCode = 1
		j.ErrorMsg = msg
		j.CompletedAt = time.Now().UTC()
	}
	j.mu.Unlock()
}

func (j *Job) appendLine(line string) {
	j.mu.Lock()
	j.ringBuffer = append(j.ringBuffer, line)
	if len(j.ringBuffer) > ringBufferSize {
		j.ringBuffer = j.ringBuffer[len(j.ringBuffer)-ringBufferSize:]
	}
	j.LineCount++
	subs := make([]chan string, 0, len(j.subscribers))
	for c := range j.subscribers {
		subs = append(subs, c)
	}
	j.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- line:
		default:
			// Drop the oldest buffered line, then retry once — a slow viewer
			// must never block the operation goroutine.
			select {
			case <-c:
			default:
			}
			select {
			case c <- line:
			default:
			}
		}
	}
}

func (j *Job) logLines() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.ringBuffer...)
}

func tPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
