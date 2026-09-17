package main

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// jobRegistry owns in-flight Jobs, drives the engine, and fans lifecycle changes
// out to the event hook (durable outbox) + fleet hub. It is the single entry
// point for every mutation.
type jobRegistry struct {
	cfg    Config
	eng    *engine
	events *eventBuffer

	hook   JobEventHook
	fleet  interface{ Trigger() }
	emitMu sync.Mutex // serialize hook emits (concurrent op goroutines)

	mu   sync.Mutex
	jobs map[string]*Job
}

func newJobRegistry(cfg Config, eng *engine, events *eventBuffer) *jobRegistry {
	return &jobRegistry{cfg: cfg, eng: eng, events: events, jobs: map[string]*Job{}}
}

func (r *jobRegistry) setHook(h JobEventHook)            { r.hook = h }
func (r *jobRegistry) setFleet(f interface{ Trigger() }) { r.fleet = f }

// fleetTrigger is a lightweight fleet-only nudge (no controller event, no
// persistence) used by the engine to broadcast live progress.
func (r *jobRegistry) fleetTrigger() {
	if r.fleet != nil {
		r.fleet.Trigger()
	}
}

func (r *jobRegistry) onChange(j *Job) func(JobEvent) {
	return func(evt JobEvent) {
		r.emitMu.Lock()
		if r.hook != nil {
			r.hook(j, evt)
		}
		r.emitMu.Unlock()
		if r.fleet != nil {
			r.fleet.Trigger()
		}
	}
}

// start registers a new job and runs it in the background. Returns immediately
// with the job id so the trigger caller can poll.
// newJob builds the in-memory Job from a request. Split out of start so the
// request→job field mapping is reachable from a test without launching the run
// goroutine: every in-memory op parameter enters the engine through exactly these
// assignments, so a dropped or crossed one here silently disables a whole feature
// while every unit test that hand-builds a Job still passes.
func newJob(id string, req JobRequest, cancel context.CancelFunc) *Job {
	return &Job{
		jobPublic: jobPublic{
			ID: id, Project: req.Project, Operation: req.Operation,
			Target: req.Target, State: JobPending, TriggerKey: req.TriggerKey, Services: req.Services,
		},
		targets:         req.Targets,
		targetIDs:       req.TargetIDs,
		force:           req.Force,
		timeout:         req.Timeout,
		overrideImage:   req.OverrideImage,
		overrideService: req.OverrideService,
		healthTimeoutS:  req.HealthTimeoutS,
		swapTimeoutS:    req.SwapTimeoutS,
		services:        req.Services,
		cancel:          cancel,
		subscribers:     map[chan string]struct{}{},
	}
}

func (r *jobRegistry) start(parentCtx context.Context, req JobRequest) *Job {
	id := uuid.NewString()
	jobCtx, cancel := context.WithCancel(parentCtx)
	j := newJob(id, req, cancel)

	r.mu.Lock()
	r.jobs[id] = j
	r.evictOldLocked()
	r.mu.Unlock()

	go func() {
		defer cancel()
		r.eng.run(jobCtx, j, r.onChange(j), r.fleetTrigger)
	}()
	return j
}

func (r *jobRegistry) get(id string) (*Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

// cancelJob cancels a running job; returns false if unknown/already terminal.
func (r *jobRegistry) cancelJob(id string) bool {
	j, ok := r.get(id)
	if !ok {
		return false
	}
	j.mu.Lock()
	terminal := j.State == JobCompleted || j.State == JobFailed || j.State == JobCancelled
	if !terminal {
		j.State = JobCancelled
		j.CompletedAt = time.Now().UTC()
	}
	cancel := j.cancel
	j.mu.Unlock()
	if terminal {
		return false
	}
	if cancel != nil {
		cancel()
	}
	r.onChange(j)(EventCancelled)
	return true
}

// list merges active in-memory jobs with recent persisted jobs (in-memory wins
// on id collision), newest first.
func (r *jobRegistry) list() []jobPublic {
	r.mu.Lock()
	active := make([]jobPublic, 0, len(r.jobs))
	seen := map[string]struct{}{}
	for id, j := range r.jobs {
		active = append(active, j.snapshot())
		seen[id] = struct{}{}
	}
	r.mu.Unlock()

	recent, _ := r.events.listRecentJobs(recentJobsRetained)
	out := active
	for _, jp := range recent {
		if _, dup := seen[jp.ID]; dup {
			continue
		}
		out = append(out, jp)
	}
	return out
}

// evictOldLocked drops terminal in-memory jobs beyond the retention window so the
// map can't grow unbounded. Caller holds r.mu.
func (r *jobRegistry) evictOldLocked() {
	if len(r.jobs) <= recentJobsRetained {
		return
	}
	type kv struct {
		id string
		at int64
	}
	var terminal []kv
	for id, j := range r.jobs {
		st := j.snapshot()
		if st.State == JobCompleted || st.State == JobFailed || st.State == JobCancelled {
			terminal = append(terminal, kv{id, st.CompletedAt.UnixNano()})
		}
	}
	for i := 0; i < len(terminal); i++ {
		for k := i + 1; k < len(terminal); k++ {
			if terminal[k].at < terminal[i].at {
				terminal[i], terminal[k] = terminal[k], terminal[i]
			}
		}
	}
	excess := len(r.jobs) - recentJobsRetained
	for i := 0; i < excess && i < len(terminal); i++ {
		delete(r.jobs, terminal[i].id)
	}
}

// --- log streaming (job log WS) ---

func (r *jobRegistry) subscribe(id string) (<-chan string, []string, func(), bool) {
	j, ok := r.get(id)
	if !ok {
		return nil, nil, nil, false
	}
	ch := make(chan string, subscriberBuffer)
	j.mu.Lock()
	backlog := append([]string(nil), j.ringBuffer...)
	j.subscribers[ch] = struct{}{}
	j.mu.Unlock()
	unsub := func() {
		j.mu.Lock()
		delete(j.subscribers, ch)
		j.mu.Unlock()
		close(ch)
	}
	return ch, backlog, unsub, true
}
