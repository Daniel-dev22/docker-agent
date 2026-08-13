package main

// Container health-wait + crash-loop detection, used by the stack-update engine.
//
// Two stages, both over the docker SDK (one ContainerInspect per container),
// with NO subprocess and NO format-string parsing:
//
//   Stage 1 — swap detection: after a recreate, wait for container IDs to change.
//     Partial-update tolerant: succeed once ≥1 container has swapped + a 20s grace
//     for the rest, or immediately when all have swapped. Missing containers
//     (mid-recreation) are expected. 0 swaps at timeout = failure (the recreate
//     didn't take). A no-diff update (previousIDs empty) skips this stage.
//   Stage 2 — health poll: wait for every monitored container to reach
//     healthy/none, failing fast on a crash loop (restart count climbed past the
//     baseline by restart_threshold) or a stopped container.
//
// The timeouts (longer on a slow host class — see engine.healthTimeouts) and the
// health_excludes / health_containers subset come from the strategy
// (resolvers.go) or from the defaults.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// swapGracePeriod is the window granted to the remaining containers after the
// first one swaps.
const swapGracePeriod = 20 * time.Second

// healthWaitParams configures one health-wait over a project's containers.
type healthWaitParams struct {
	containers  []string          // actual container names to monitor
	previousIDs map[string]string // name → prior container ID; empty/nil = skip swap phase
	excluded    map[string]bool   // names to skip in the health phase (no shell/healthcheck)
	// onlyHealth, when non-empty, restricts the health phase to this subset (a
	// stack may only have one meaningful probe); every other container is excluded.
	onlyHealth       []string
	swapTimeout      time.Duration
	healthTimeout    time.Duration
	restartThreshold int
}

// healthWaitResult reports the outcome. err non-nil = the wait failed (the engine
// rolls back). unhealthy is the set the caller dumps logs for.
type healthWaitResult struct {
	healthMap map[string]string
	unhealthy []string
	crashLoop bool
	elapsed   time.Duration
}

// inspectAll inspects every name concurrently, returning a state map and an
// error map (missing/inspect-failed). Bounded by the engine's bulk concurrency
// so a big stack doesn't hammer the socket.
func (e *engine) inspectAll(ctx context.Context, names []string) (map[string]containerState, map[string]error) {
	states := make(map[string]containerState, len(names))
	errs := map[string]error{}
	var mu sync.Mutex
	sem := make(chan struct{}, e.bulkConcurrency)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ictx, cancel := context.WithTimeout(ctx, 8*time.Second)
			st, err := e.docker.inspectState(ictx, name)
			cancel()
			mu.Lock()
			if err != nil {
				errs[name] = err
			} else {
				states[name] = st
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	return states, errs
}

// healthWait runs the swap phase (when previousIDs is set) then the health phase,
// streaming progress into the job log. Returns an error on swap/health failure.
func (e *engine) healthWait(ctx context.Context, j *Job, p healthWaitParams) (healthWaitResult, error) {
	if p.restartThreshold <= 0 {
		p.restartThreshold = 3
	}
	start := time.Now()
	var res healthWaitResult

	// Effective exclusion set: explicit excludes ∪ (everything outside onlyHealth).
	excluded := map[string]bool{}
	for n, v := range p.excluded {
		if v {
			excluded[n] = true
		}
	}
	if len(p.onlyHealth) > 0 {
		keep := map[string]bool{}
		for _, n := range p.onlyHealth {
			keep[n] = true
		}
		for _, n := range p.containers {
			if !keep[n] {
				excluded[n] = true
			}
		}
	}

	if len(p.previousIDs) > 0 {
		if err := e.waitForSwap(ctx, j, p, start); err != nil {
			res.elapsed = time.Since(start)
			return res, err
		}
	}

	res = e.waitForHealth(ctx, j, p, excluded, p.restartThreshold)
	res.elapsed = time.Since(start)
	if len(res.unhealthy) > 0 {
		return res, fmt.Errorf("unhealthy after update: %s", strings.Join(res.unhealthy, ", "))
	}
	return res, nil
}

// waitForSwap waits for container IDs to change after a recreate (partial-update
// tolerant — see file header).
func (e *engine) waitForSwap(ctx context.Context, j *Job, p healthWaitParams, start time.Time) error {
	swapped := map[string]bool{}
	seenMissing := map[string]bool{}
	var firstSwap time.Time
	deadline := start.Add(p.swapTimeout)

	j.appendLine(fmt.Sprintf("waiting for container swap (timeout %s)", p.swapTimeout))
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled during swap wait")
		}
		states, errs := e.inspectAll(ctx, p.containers)
		for _, name := range p.containers {
			if swapped[name] {
				continue
			}
			prev, known := p.previousIDs[name]
			if !known {
				swapped[name] = true // brand-new container in this update
				continue
			}
			if _, missing := errs[name]; missing {
				seenMissing[name] = true
				continue
			}
			st := states[name]
			if st.ID != prev {
				swapped[name] = true
				if firstSwap.IsZero() {
					firstSwap = time.Now()
				}
			} else if seenMissing[name] {
				swapped[name] = true // came back with same ID after going missing — edge case
				if firstSwap.IsZero() {
					firstSwap = time.Now()
				}
			}
		}

		if len(swapped) == len(p.containers) {
			j.appendLine(fmt.Sprintf("all %d container(s) swapped", len(p.containers)))
			return nil
		}
		if !firstSwap.IsZero() && time.Since(firstSwap) >= swapGracePeriod {
			j.appendLine(fmt.Sprintf("%d/%d swapped, grace period elapsed — proceeding", len(swapped), len(p.containers)))
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		if !sleepCtx(ctx, time.Second) {
			return fmt.Errorf("cancelled during swap wait")
		}
	}

	if len(swapped) > 0 {
		j.appendLine(fmt.Sprintf("%d/%d swapped at timeout — accepting partial update", len(swapped), len(p.containers)))
		return nil // partial swap acceptable (some services had no image change)
	}

	// 0 swaps — report the stuck containers for diagnosis.
	states, errs := e.inspectAll(ctx, p.containers)
	var notSwapped []string
	for _, name := range p.containers {
		if swapped[name] {
			continue
		}
		if _, missing := errs[name]; missing {
			notSwapped = append(notSwapped, name+" (missing)")
			continue
		}
		notSwapped = append(notSwapped, fmt.Sprintf("%s (same ID: %s)", name, shortID(states[name].ID)))
	}
	return fmt.Errorf("timeout waiting for container swap; not swapped: %s", strings.Join(notSwapped, ", "))
}

// waitForHealth polls until every monitored container is healthy/none, failing
// fast on a crash loop or a stopped container.
func (e *engine) waitForHealth(ctx context.Context, j *Job, p healthWaitParams, excluded map[string]bool, threshold int) healthWaitResult {
	start := time.Now()
	deadline := start.Add(p.healthTimeout)
	healthMap := map[string]string{}

	// Baseline restart counts (to detect post-update crash loops).
	initialRestarts := map[string]int{}
	states, _ := e.inspectAll(ctx, p.containers)
	for name, st := range states {
		initialRestarts[name] = st.RestartCount
	}

	j.appendLine(fmt.Sprintf("waiting for health (timeout %s, restart threshold %d)", p.healthTimeout, threshold))
	for {
		if ctx.Err() != nil {
			return healthWaitResult{healthMap: healthMap, unhealthy: notHealthy(healthMap, excluded)}
		}
		states, errs := e.inspectAll(ctx, p.containers)
		allHealthy := true
		crashLoop := false
		for _, name := range p.containers {
			if excluded[name] {
				healthMap[name] = "excluded"
				continue
			}
			if err, bad := errs[name]; bad {
				healthMap[name] = "error"
				_ = err
				allHealthy = false
				continue
			}
			st := states[name]
			if st.RestartCount > initialRestarts[name]+threshold {
				healthMap[name] = "crash_loop"
				crashLoop = true
				allHealthy = false
				continue
			}
			if !st.Running {
				healthMap[name] = "not_running"
				allHealthy = false
				continue
			}
			healthMap[name] = st.Health
			if st.Health != "healthy" && st.Health != "none" {
				allHealthy = false
			}
		}

		if allHealthy {
			j.appendLine("all monitored containers healthy")
			return healthWaitResult{healthMap: healthMap}
		}
		if crashLoop {
			u := notHealthy(healthMap, excluded)
			j.appendLine("crash loop detected: " + strings.Join(u, ", "))
			return healthWaitResult{healthMap: healthMap, unhealthy: u, crashLoop: true}
		}
		if time.Now().After(deadline) {
			break
		}
		if !sleepCtx(ctx, time.Second) {
			return healthWaitResult{healthMap: healthMap, unhealthy: notHealthy(healthMap, excluded)}
		}
	}

	u := notHealthy(healthMap, excluded)
	j.appendLine("timeout waiting for health; unhealthy: " + strings.Join(u, ", "))
	return healthWaitResult{healthMap: healthMap, unhealthy: u}
}

// notHealthy returns the sorted set of monitored containers not in healthy/none/
// excluded — the rollback target + the log-dump list.
func notHealthy(healthMap map[string]string, excluded map[string]bool) []string {
	var out []string
	for name, status := range healthMap {
		if excluded[name] {
			continue
		}
		if status != "healthy" && status != "none" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// sleepCtx sleeps for d unless ctx is cancelled first; returns false on cancel.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
