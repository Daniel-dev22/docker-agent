package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// nasEngine is the host class that actually failed: "nas" is NOT in
// slowHostClasses, so it gets the short 120s/180s pair. Every case below is
// anchored on that, because the whole feature exists to override it.
func nasEngine() *engine {
	return &engine{cfg: Config{NodeName: "nas01"}, composeOpTimeout: 30 * time.Minute}
}

func TestEffectiveHealthTimeoutsPrecedence(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")

	cases := []struct {
		name             string
		meta             resolveMeta
		job              *Job
		wantSwap, wantHl time.Duration
	}{
		{
			name:     "node-class default when nothing overrides",
			wantSwap: 120 * time.Second, wantHl: 180 * time.Second,
		},
		{
			name:     "project row beats the node class",
			meta:     resolveMeta{swapTimeoutS: 200, healthTimeoutS: 900},
			wantSwap: 200 * time.Second, wantHl: 900 * time.Second,
		},
		{
			name:     "request beats the project row",
			meta:     resolveMeta{swapTimeoutS: 200, healthTimeoutS: 900},
			job:      &Job{swapTimeoutS: 240, healthTimeoutS: 1200},
			wantSwap: 240 * time.Second, wantHl: 1200 * time.Second,
		},
		{
			name:     "request health only — swap still falls back to the row",
			meta:     resolveMeta{swapTimeoutS: 200, healthTimeoutS: 900},
			job:      &Job{healthTimeoutS: 1200},
			wantSwap: 200 * time.Second, wantHl: 1200 * time.Second,
		},
		{
			// Negative control: a ZERO must not be read as "override with 0".
			// A zero-valued knob that disabled the health wait would turn every
			// update into an unverified deploy, silently.
			name:     "zeros are absent, not an override to zero",
			meta:     resolveMeta{swapTimeoutS: 0, healthTimeoutS: 0},
			job:      &Job{swapTimeoutS: 0, healthTimeoutS: 0},
			wantSwap: 120 * time.Second, wantHl: 180 * time.Second,
		},
		{
			// Negative control: a negative value is not an override either, and
			// must not produce a negative deadline (which fires immediately).
			name:     "negatives are ignored",
			meta:     resolveMeta{swapTimeoutS: -1, healthTimeoutS: -600},
			job:      &Job{healthTimeoutS: -5},
			wantSwap: 120 * time.Second, wantHl: 180 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h := nasEngine().effectiveHealthTimeouts(tc.job, tc.meta, "update")
			if s != tc.wantSwap || h != tc.wantHl {
				t.Fatalf("got swap=%v health=%v, want swap=%v health=%v", s, h, tc.wantSwap, tc.wantHl)
			}
		})
	}
}

// The clamp exists because a health budget at or above the whole-op budget is not
// a longer wait — the op deadline fires first, and it fires during the health
// phase, which is the one place a timeout costs the rollback.
func TestEffectiveHealthTimeoutsClampedToOpBudget(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")
	e := nasEngine() // 30m op budget, 5m reserve → 25m for swap+health
	j := &Job{}
	s, h := e.effectiveHealthTimeouts(j, resolveMeta{swapTimeoutS: 120, healthTimeoutS: 3600}, "update")
	if s+h > e.composeOpTimeout-healthBudgetReserve {
		t.Fatalf("swap+health = %v exceeds the clamp budget %v", s+h, e.composeOpTimeout-healthBudgetReserve)
	}
	// Health is the half an operator meant to lengthen, so it survives and swap
	// yields — the reverse of the first implementation.
	if h < 20*time.Minute {
		t.Fatalf("clamp sacrificed the health half: health=%v swap=%v", h, s)
	}
	if !containsLine(j, "clamped") {
		t.Fatal("clamping must be announced on the job log, not applied silently")
	}
	if !containsLine(j, h.String()) {
		t.Fatalf("the clamp must announce the RESULTING value; log = %v", j.logLines())
	}
}

// REGRESSION. The first implementation collapsed health to ZERO whenever swap alone
// reached the budget — reachable with a single router-legal row value
// (swap_timeout_s: 1800). A zero health budget is not a short wait: waitForHealth
// sets `deadline = start.Add(0)`, polls once, and a just-recreated container reports
// `starting`, so EVERY stack with a healthcheck fails and rolls back — and the
// rollback, given the same zero, reports itself unhealthy too. An operator who
// RAISED a budget would have broken every deploy on the host.
func TestClampNeverStarvesTheHealthWait(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")

	for _, swapS := range []int{1400, 1500, 1800, 86400} {
		e := nasEngine()
		s, h := e.effectiveHealthTimeouts(&Job{}, resolveMeta{swapTimeoutS: swapS}, "update")
		if h < minHealthBudget {
			t.Fatalf("swap_timeout_s=%d produced health=%v, below the %v floor (swap=%v)",
				swapS, h, minHealthBudget, s)
		}
		if s < 0 {
			t.Fatalf("swap_timeout_s=%d produced a negative swap budget %v", swapS, s)
		}
	}
}

// A value large enough to overflow time.Duration must be REFUSED, not truncated.
// Overflow yields a negative duration, which sails past the clamp (a negative sum is
// never over budget) and produces an already-expired deadline — the rollback then
// aborts AFTER the disk tags were reverted, leaving disk and runtime disagreeing.
// The request path is the one that matters here: it reaches the agent unvalidated.
func TestBudgetOfRefusesOverflowAndNonPositive(t *testing.T) {
	for _, seconds := range []int{0, -1, -600, 1 << 62, 9223372037} {
		if d, ok := budgetOf(seconds); ok {
			t.Fatalf("budgetOf(%d) accepted, returning %v", seconds, d)
		}
	}
	if d, ok := budgetOf(600); !ok || d != 10*time.Minute {
		t.Fatalf("budgetOf(600) = %v, %v; want 10m, true", d, ok)
	}
}

// With no op deadline configured (DOCKER_COMPOSE_OP_TIMEOUT=0, the documented "no
// timeout" setting) there is nothing to clamp against — but maxBudget must still
// bound what a single value can be, or a rollback deadline runs for a day.
func TestNoOpTimeoutStillBounded(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")
	e := &engine{cfg: Config{NodeName: "nas01"}, composeOpTimeout: 0}
	_, h := e.effectiveHealthTimeouts(&Job{}, resolveMeta{healthTimeoutS: 200000}, "update")
	if h > maxBudget {
		t.Fatalf("health=%v exceeds maxBudget=%v with no op timeout", h, maxBudget)
	}
}

// A small op budget is exactly where the clamp matters most, and a flat 5m reserve
// made it inert there (budget <= 0 → no clamp at all).
func TestClampWorksOnASmallOpBudget(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")
	e := &engine{cfg: Config{NodeName: "nas01"}, composeOpTimeout: 6 * time.Minute}
	s, h := e.effectiveHealthTimeouts(&Job{}, resolveMeta{}, "update")
	if s+h > e.composeOpTimeout {
		t.Fatalf("swap+health = %v exceeds the whole op budget %v", s+h, e.composeOpTimeout)
	}
	if h < minHealthBudget {
		t.Fatalf("small op budget starved the health wait: %v", h)
	}
}

// The source of the budget must reach the job log — an operator reading a failed
// job needs to know WHICH of the three levels applied, otherwise "timeout 3m0s"
// is indistinguishable from an override that silently failed to take effect.
func TestEffectiveHealthTimeoutsLogsItsSource(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")

	j := &Job{healthTimeoutS: 1200}
	nasEngine().effectiveHealthTimeouts(j, resolveMeta{healthTimeoutS: 900}, "update")
	if !containsLine(j, "(request)") {
		t.Fatalf("request-sourced budget not announced; log = %v", j.logLines())
	}

	j2 := &Job{}
	nasEngine().effectiveHealthTimeouts(j2, resolveMeta{healthTimeoutS: 900}, "update")
	if !containsLine(j2, "(project)") {
		t.Fatalf("project-sourced budget not announced; log = %v", j2.logLines())
	}

	// Negative control: the plain node-class default is the overwhelmingly common
	// case and must stay silent, or the line becomes noise nobody reads.
	j3 := &Job{}
	nasEngine().effectiveHealthTimeouts(j3, resolveMeta{}, "update")
	if containsLine(j3, "health budget:") {
		t.Fatalf("node-class default should not announce itself; log = %v", j3.logLines())
	}
}

// A nil job is the read-only/detection path. It must resolve budgets without
// panicking on the log calls.
func TestEffectiveHealthTimeoutsNilJob(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")
	s, h := nasEngine().effectiveHealthTimeouts(nil, resolveMeta{healthTimeoutS: 900}, "update")
	if s != 120*time.Second || h != 900*time.Second {
		t.Fatalf("nil-job resolution wrong: swap=%v health=%v", s, h)
	}
}

// The rollback's total deadline. Extracted as a pure function because the rollback
// itself cannot be unit-tested today (engine.compose is a concrete *composeBackend,
// not an interface), and a deadline derived from operator-supplied numbers is the
// part most likely to be got wrong.
//
// The floor is the point: a deliberately SHORT budget ("fail this stack fast")
// would otherwise leave the rollback seconds to revert tags, reload and recreate.
// Being cancelled part-way leaves disk pinned to the old image and the runtime on
// the new failing one — worse than not rolling back at all.
func TestRollbackDeadlineHasAFloor(t *testing.T) {
	if got := rollbackDeadline(10*time.Second, 30*time.Second); got != minRollbackDeadline {
		t.Fatalf("a tiny budget produced a %v rollback deadline; want the %v floor", got, minRollbackDeadline)
	}
	// Negative control: a generous budget must NOT be shrunk to the floor.
	long := rollbackDeadline(3*time.Minute, 20*time.Minute)
	if long != 3*time.Minute+20*time.Minute+rollbackSlack {
		t.Fatalf("a large budget was not honoured: %v", long)
	}
	if long <= minRollbackDeadline {
		t.Fatal("the negative control is not exercising the above-floor branch")
	}
}

func containsLine(j *Job, sub string) bool {
	for _, l := range j.logLines() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// REGRESSION. selectResolver used to return the moment override_image was set,
// BEFORE the project row was read — so a pinned-image deploy silently ran on the
// node-class defaults, losing the budget, the rollback scope and the health
// excludes.
//
// That is not an edge case. override_image is how a freshly BUILT image reaches a
// stack (control-api's build→swap posts exactly that), so the stack configured with
// a long budget got the short one on the single deploy whose first boot is slowest —
// which is the deploy the whole feature exists for.
func TestPinnedImageDeployKeepsTheProjectPolicy(t *testing.T) {
	ic := &imageChecker{overrides: map[string]strategyOverride{
		"frigate": {
			Key: "frigate", MatchType: "project",
			HealthTimeoutS: 600, SwapTimeoutS: 180,
			RollbackScope:  "whole-stack",
			HealthExcludes: []string{"sidecar"},
		},
	}}
	e := &engine{cfg: Config{NodeName: "nas01"}, composeOpTimeout: 30 * time.Minute, images: ic}

	resolver, meta := e.selectResolver("frigate", "registry/frigate:0.19-abc1234", "frigate")

	// The request still chooses the resolver...
	if resolver.kind() != "override" {
		t.Fatalf("override_image must still select the override resolver, got %q", resolver.kind())
	}
	// ...but the row still supplies the policy.
	if meta.healthTimeoutS != 600 || meta.swapTimeoutS != 180 {
		t.Fatalf("pinned deploy lost the budget: health=%d swap=%d", meta.healthTimeoutS, meta.swapTimeoutS)
	}
	if meta.rollbackScope != "whole-stack" {
		t.Fatalf("pinned deploy lost the rollback scope: %q", meta.rollbackScope)
	}
	if len(meta.healthExcludes) != 1 || meta.healthExcludes[0] != "sidecar" {
		t.Fatalf("pinned deploy lost the health excludes: %v", meta.healthExcludes)
	}
}

// Negative control for the test above: with NO row, a pinned deploy must fall back
// to the defaults rather than inventing a policy. Without this, the test above is
// satisfied by a selectResolver that hard-codes those values.
func TestPinnedImageDeployWithNoRowUsesDefaults(t *testing.T) {
	e := &engine{cfg: Config{NodeName: "nas01"}, composeOpTimeout: 30 * time.Minute,
		images: &imageChecker{overrides: map[string]strategyOverride{}}}
	_, meta := e.selectResolver("frigate", "registry/frigate:x", "frigate")
	if meta.healthTimeoutS != 0 || meta.swapTimeoutS != 0 {
		t.Fatalf("invented a budget with no row: health=%d swap=%d", meta.healthTimeoutS, meta.swapTimeoutS)
	}
	if meta.rollbackScope != "per-container" {
		t.Fatalf("rollback scope default changed: %q", meta.rollbackScope)
	}
}

func TestValidBudgetSeconds(t *testing.T) {
	for _, v := range []int{0, 1, 600, int(maxBudget / time.Second)} {
		if !validBudgetSeconds(v) {
			t.Fatalf("validBudgetSeconds(%d) = false, want true", v)
		}
	}
	for _, v := range []int{-1, int(maxBudget/time.Second) + 1, 9223372037, 1 << 62} {
		if validBudgetSeconds(v) {
			t.Fatalf("validBudgetSeconds(%d) = true, want false", v)
		}
	}
}

// The wire contract. Every test above builds a Job or a resolveMeta by hand, which
// says nothing about whether a value posted by a caller ever ARRIVES there — a
// renamed JSON tag, or a handler that forgets to forward the field, would leave the
// whole feature inert with the suite still green.
func TestRequestBudgetReachesTheJob(t *testing.T) {
	var body composeOpBody
	raw := `{"op":"update","health_timeout_s":1200,"swap_timeout_s":240}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.HealthTimeoutS != 1200 || body.SwapTimeoutS != 240 {
		t.Fatalf("wire → body lost the budget: health=%d swap=%d", body.HealthTimeoutS, body.SwapTimeoutS)
	}

	// body → JobRequest → Job through the PRODUCTION constructor, not a copy of it.
	// Re-typing the assignment here would test the test.
	req := JobRequest{Operation: body.Op, Project: "p",
		HealthTimeoutS: body.HealthTimeoutS, SwapTimeoutS: body.SwapTimeoutS}
	j := newJob("id", req, func() {})

	// Asserting the two are DIFFERENT numbers is deliberate: equal values would let
	// a crossed assignment pass.
	if j.healthTimeoutS != 1200 || j.swapTimeoutS != 240 {
		t.Fatalf("request → job lost or crossed the budget: health=%d swap=%d", j.healthTimeoutS, j.swapTimeoutS)
	}

	swap, health := nasEngine().effectiveHealthTimeouts(j, resolveMeta{}, "update")
	if health != 20*time.Minute || swap != 4*time.Minute {
		t.Fatalf("job → resolution wrong: swap=%v health=%v", swap, health)
	}
}

// The other half of the same contract: the strategy row's JSON, as the router
// actually serialises it, must land in resolveMeta.
func TestProjectRowBudgetReachesMeta(t *testing.T) {
	var o strategyOverride
	raw := `{"key":"frigate","match_type":"project","version_source":"github-branch",
	         "health_timeout_s":900,"swap_timeout_s":200}`
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if o.HealthTimeoutS != 900 || o.SwapTimeoutS != 200 {
		t.Fatalf("wire → override lost the budget: %+v", o)
	}

	e := &engine{cfg: Config{NodeName: "nas01"}, composeOpTimeout: 30 * time.Minute,
		images: &imageChecker{overrides: map[string]strategyOverride{"frigate": o}}}
	_, meta := e.selectResolver("frigate", "", "")
	if meta.healthTimeoutS != 900 || meta.swapTimeoutS != 200 {
		t.Fatalf("override → meta lost or crossed the budget: health=%d swap=%d",
			meta.healthTimeoutS, meta.swapTimeoutS)
	}
}

// A nil job on the clamping path. appendLine takes j.mu, so an unguarded log call
// panics — and the clamp is exactly what a large project row triggers, which is the
// combination most likely to reach the read-only detection path.
func TestClampWithNilJobDoesNotPanic(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")
	swap, health := nasEngine().effectiveHealthTimeouts(nil, resolveMeta{healthTimeoutS: 3600}, "update")
	if health < minHealthBudget || swap < 0 {
		t.Fatalf("nil-job clamp produced swap=%v health=%v", swap, health)
	}
}

// Where the env override sits. DOCKER_HEALTH_TIMEOUT is a NODE-wide default, so a
// project row is more specific and outranks it. Stating that here means the ordering
// is a decision rather than an accident of which if-block came last.
func TestEnvOverrideIsANodeDefaultAndSitsBelowTheRow(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "7m")
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")

	_, envOnly := nasEngine().effectiveHealthTimeouts(&Job{}, resolveMeta{}, "update")
	if envOnly != 7*time.Minute {
		t.Fatalf("env default not applied: %v", envOnly)
	}
	_, withRow := nasEngine().effectiveHealthTimeouts(&Job{}, resolveMeta{healthTimeoutS: 900}, "update")
	if withRow != 15*time.Minute {
		t.Fatalf("project row did not outrank the node-wide env default: %v", withRow)
	}
}

// An aborted health wait must be reported as a FAILURE. On the first iteration
// healthMap is empty, so notHealthy returns nothing — and an empty unhealthy set is
// indistinguishable from "everything came up healthy". Without the aborted arm the
// engine marks an update completed having verified nothing at all.
func TestHealthVerdict(t *testing.T) {
	// The dangerous case: aborted, with NOTHING in the unhealthy set.
	if err := healthVerdict(healthWaitResult{healthMap: map[string]string{}, aborted: true},
		context.DeadlineExceeded); err == nil {
		t.Fatal("an aborted wait with an empty unhealthy set was reported as success")
	}
	// Aborted with no context error still fails — the flag is the authority, not
	// whether we can name a cause.
	if err := healthVerdict(healthWaitResult{aborted: true}, nil); err == nil {
		t.Fatal("aborted must fail even when the context error is already cleared")
	}
	// A real unhealthy verdict names the containers.
	err := healthVerdict(healthWaitResult{unhealthy: []string{"frigate"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "frigate") {
		t.Fatalf("unhealthy verdict = %v; want it to name the container", err)
	}
	// Negative control: a clean wait must pass, or every one of the above is
	// satisfied by a function that always returns an error.
	if err := healthVerdict(healthWaitResult{healthMap: map[string]string{"web": "healthy"}}, nil); err != nil {
		t.Fatalf("a healthy wait was reported as a failure: %v", err)
	}
}

// An excluded container that is alive but cannot report a verdict is fine. One that
// is crash-looping or stopped is not — filtering by NAME dropped those, so the wait
// ran to its deadline and then reported an empty unhealthy set, i.e. success.
func TestNotHealthyFiltersByStatusNotByName(t *testing.T) {
	excl := map[string]bool{"portainer": true, "sidecar": true}
	got := notHealthy(map[string]string{
		"web":       "healthy",
		"portainer": "excluded",    // alive, no verdict — correctly ignored
		"sidecar":   "not_running", // excluded BY NAME but genuinely broken
	}, excl)
	if len(got) != 1 || got[0] != "sidecar" {
		t.Fatalf("got %v; a broken excluded container must still be reported", got)
	}
}
