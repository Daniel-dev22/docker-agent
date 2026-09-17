package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestEveryRefusalCarriesACode drives each deterministic 4xx of the refusal
// routes; auditRefusalCodes (on every capEnv) fails the test for any without a
// code, and each case pins the code a consumer keys on.
func TestEveryRefusalCarriesACode(t *testing.T) {
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
		return append(defaultContainers(root),
			fakeContainer{id: "ab" + strings.Repeat("1", 62), name: "one"},
			fakeContainer{id: "ab" + strings.Repeat("2", 62), name: "two"})
	})
	withIdempotency(t, e, t.TempDir())
	e.writeCompose(t, "owned", "services: {}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned"), ComposeFiles: []string{"docker-compose.yml"}}))
	ghost := filepath.Join(e.root, "ghost")
	manyIDs := make([]string, maxContainerTargets+1)
	for i := range manyIDs {
		manyIDs[i] = fmt.Sprintf("c%03d", i)
	}

	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"op-bind", "POST", "/v1/projects/owned/op", `{"op":`, 400, "invalid_body"},
		{"op-invalid", "POST", "/v1/projects/owned/op", `{"op":"explode"}`, 400, "invalid_op"},
		{"op-unknown-project", "POST", "/v1/projects/nope/op", `{"op":"up"}`, 404, "unknown_project"},
		{"op-budget", "POST", "/v1/projects/owned/op", `{"op":"update","health_timeout_s":-5}`, 400, "invalid_budget"},
		{"op-override-image", "POST", "/v1/projects/owned/op", `{"op":"update","override_image":"a b"}`, 400, "invalid_override_image"},
		{"register-bind", "POST", "/v1/projects", `[1]`, 400, "invalid_body"},
		{"register-no-name", "POST", "/v1/projects", `{"files":{"docker-compose.yml":""}}`, 400, "invalid_project_name"},
		{"register-bad-name", "POST", "/v1/projects", `{"name":"Bad","files":{"docker-compose.yml":""}}`, 400, "invalid_project_name"},
		{"register-long-name", "POST", "/v1/projects", `{"name":"` + strings.Repeat("a", maxProjectNameLen+1) + `","files":{"docker-compose.yml":""}}`, 400, "invalid_project_name"},
		{"register-no-dir-or-files", "POST", "/v1/projects", `{"name":"x"}`, 400, "invalid_body"},
		{"register-relative-dir", "POST", "/v1/projects", `{"name":"x","working_dir":"rel"}`, 400, "invalid_working_dir"},
		{"register-long-dir", "POST", "/v1/projects", `{"name":"x","working_dir":"/` + strings.Repeat("d", maxPathLen) + `"}`, 400, "invalid_working_dir"},
		{"register-path-escape", "POST", "/v1/projects", `{"name":"x","working_dir":"/srv/x","env_files":["/etc/passwd"]}`, 400, "project_path_outside_working_dir"},
		{"register-bad-file-path", "POST", "/v1/projects", `{"name":"x","files":{"":""}}`, 400, "invalid_file_path"},
		{"register-long-file-component", "POST", "/v1/projects", `{"name":"x","files":{"` + strings.Repeat("f", maxProjectNameLen+1) + `":""}}`, 400, "invalid_file_path"},
		{"register-compose-missing", "POST", "/v1/projects", `{"name":"ghost","working_dir":"` + ghost + `"}`, 400, "compose_files_missing"},
		{"register-exists", "POST", "/v1/projects", `{"name":"owned","files":{"docker-compose.yml":""}}`, 409, "project_exists"},
		{"copy-bind", "POST", "/v1/projects/owned/copy", `{"new_name":`, 400, "invalid_body"},
		{"copy-no-name", "POST", "/v1/projects/owned/copy", `{}`, 400, "invalid_project_name"},
		{"copy-exists", "POST", "/v1/projects/owned/copy", `{"new_name":"owned"}`, 409, "project_exists"},
		{"copy-unknown", "POST", "/v1/projects/nope/copy", `{"new_name":"fresh"}`, 404, "unknown_project"},
		{"bundle-unknown", "GET", "/v1/projects/nope/bundle", ``, 404, "unknown_project"},
		{"bulk-bind", "POST", "/v1/containers/bulk", `{"ids":`, 400, "invalid_body"},
		{"bulk-action", "POST", "/v1/containers/bulk", `{"action":"explode","ids":["x"]}`, 400, "invalid_action"},
		{"bulk-empty", "POST", "/v1/containers/bulk", `{"action":"stop","ids":[]}`, 400, "invalid_body"},
		{"bulk-too-many", "POST", "/v1/containers/bulk", mustJSON(t, map[string]any{"action": "stop", "ids": manyIDs}), 400, "too_many_targets"},
		{"container-ref-too-long", "POST", "/v1/containers/" + strings.Repeat("r", maxProjectNameLen+1) + "/stop", ``, 400, "invalid_target"},
		{"container-ambiguous", "POST", "/v1/containers/ab/stop", ``, 400, "invalid_target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.doKeyed(t, c.method, c.path, "", c.body)
			if r.status != c.status || r.code() != c.code {
				t.Fatalf("got %d %s, want %d %s", r.status, clipTo(string(r.body), 300), c.status, c.code)
			}
		})
	}
	t.Run("idempotency-refusals", func(t *testing.T) {
		if r := e.doKeyed(t, "POST", "/v1/containers/gdrive-agent/stop", "bad\x01key", ""); r.status != 400 || r.code() != "invalid_idempotency_key" {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		big := `{"action":"stop","ids":["` + strings.Repeat("x", maxJobRequestBytes) + `"]}`
		if r := e.doKeyed(t, "POST", "/v1/containers/bulk", "k-big", big); r.status != http.StatusRequestEntityTooLarge || r.code() != "request_too_large" {
			t.Fatalf("got %d %s", r.status, clipTo(string(r.body), 300))
		}
		if n := idemRows(t, e.a.events.DB(), "key = 'k-big'"); n != 0 {
			t.Fatalf("an oversized request was claimed (%d rows)", n)
		}
		if r := e.doKeyed(t, "POST", "/v1/projects/owned/op", "k-re", `{"op":"restart"}`); r.status != 202 {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		if r := e.doKeyed(t, "POST", "/v1/projects/owned/op", "k-re", `{"op":"down"}`); r.status != 422 || r.code() != "idempotency_key_reused" {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	e.waitJobs(t)
}

// TestRegisterWriteFailureIsTransient: once the request is validated, a failing
// write is I/O — a 500 the caller may retry, never a coded refusal.
func TestRegisterWriteFailureIsTransient(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, os.Chmod(e.root, 0o555))
	t.Cleanup(func() { _ = os.Chmod(e.root, 0o755) })
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "fresh", "files": map[string]string{"docker-compose.yml": "services: {}\n"}})
	if status != http.StatusInternalServerError || body["code"] != nil {
		t.Fatalf("got %d %v, want a code-less 500", status, body)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}

// TestRefusalEchoIsBounded: a request can no longer make a refusal — and so a
// recorded answer — carry megabytes of its own input.
func TestRefusalEchoIsBounded(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	const bound = 4096
	huge := strings.Repeat("A", 1_000_000)
	cases := []struct{ name, method, path, body string }{
		{"invalid-name", "POST", "/v1/projects", `{"name":"` + huge + `","files":{"docker-compose.yml":""}}`},
		{"valid-but-long-name", "POST", "/v1/projects", `{"name":"` + strings.ToLower(huge) + `","files":{"docker-compose.yml":""}}`},
		{"long-working-dir", "POST", "/v1/projects", `{"name":"x","working_dir":"/` + huge + `"}`},
		{"long-declared-path", "POST", "/v1/projects", `{"name":"x","working_dir":"/srv/x","env_files":["/` + huge + `"]}`},
		{"long-file-path", "POST", "/v1/projects", `{"name":"x","files":{"../` + huge + `":""}}`},
		{"long-copy-name", "POST", "/v1/projects/nope/copy", `{"new_name":"` + huge + `"}`},
		{"long-bulk-refs", "POST", "/v1/containers/bulk", `{"action":"stop","ids":["` + huge[:300_000] + `","` + huge[:300_001] + `"]}`},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := fmt.Sprintf("k-echo-%d", i)
			r := e.doKeyed(t, c.method, c.path, key, c.body)
			if r.status < 400 || r.status >= 500 || r.code() == nil {
				t.Fatalf("got %d %s", r.status, clipTo(string(r.body), 300))
			}
			if len(r.body) > bound {
				t.Fatalf("a %d-byte refusal answered a %d-byte request", len(r.body), len(c.body))
			}
			var stored int
			must(t, e.a.events.DB().QueryRow(`SELECT LENGTH(body) FROM idempotency_keys WHERE key = ?`, key).Scan(&stored))
			if stored > bound {
				t.Fatalf("recorded a %d-byte answer", stored)
			}
		})
	}
}

func TestIdempotencyByteCeiling(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	db, store := e.a.events.DB(), e.a.idem
	big := bytes.Repeat([]byte("x"), 100_000)
	tx, err := db.Begin()
	must(t, err)
	for i := range 40 { // 4 MB of answers, 40 rows: far below the row cap
		_, err := tx.Exec(`INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, status, body, created_at_ns, proc)
			VALUES ('POST', 'x', ?, 'fp', 'done', 409, ?, ?, 'previous-process')`, fmt.Sprintf("big-%02d", i), big, time.Now().UnixNano())
		must(t, err)
	}
	_, err = tx.Exec(`INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, created_at_ns, proc)
		VALUES ('POST', 'x', 'live', 'fp', 'in_flight', ?, 'previous-process')`, time.Now().UnixNano())
	must(t, err)
	must(t, tx.Commit())

	if _, err := store.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	var total int
	must(t, db.QueryRow(`SELECT COALESCE(SUM(LENGTH(COALESCE(body,'')) + LENGTH(key)), 0) FROM idempotency_keys WHERE state = 'done'`).Scan(&total))
	if total > idempotencyMaxBodyBytes {
		t.Fatalf("%d bytes of answers kept, ceiling %d", total, idempotencyMaxBodyBytes)
	}
	if idemRows(t, db, "key = 'big-39'") != 1 || idemRows(t, db, "key = 'big-00'") != 0 {
		t.Fatal("the byte ceiling must evict the oldest answers first")
	}
	if idemRows(t, db, "key = 'live'") != 1 {
		t.Fatal("an in-flight claim was evicted")
	}
}

// TestIdempotencyWorstCaseSize measures the database file at steady state with both
// caps binding: worst-case keys, answers sized so rows AND bytes are at their caps,
// and repeated turnover so freed pages are part of the figure.
func TestIdempotencyWorstCaseSize(t *testing.T) {
	if testing.Short() {
		t.Skip("fills the table three times over")
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "events.sqlite")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	must(t, err)
	defer db.Close()
	s, err := newIdempotencyStore(db)
	must(t, err)
	size := func() int64 {
		_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		var pages, pageSize int64
		must(t, db.QueryRow(`PRAGMA page_count`).Scan(&pages))
		must(t, db.QueryRow(`PRAGMA page_size`).Scan(&pageSize))
		return pages * pageSize
	}
	perRowBody := idempotencyMaxBodyBytes/idempotencyMaxRows - idempotencyMaxKeyLen
	body := bytes.Repeat([]byte("b"), perRowBody)
	var peak int64
	for i := range 3 * idempotencyMaxRows {
		key := fmt.Sprintf("%0128d", i)
		scope := pathSHA(fmt.Sprintf("/v1/containers/c%d/stop", i))
		fp := strings.Repeat("f", 64)
		_, _, err := s.claim(context.Background(), "DELETE", scope, key, fp)
		must(t, err)
		must(t, s.record(context.Background(), pendingAnswer{method: "DELETE", pathSHA: scope, key: key, fp: fp, status: 409, body: body}))
		if i%500 == 499 {
			_, err := s.prune(context.Background())
			must(t, err)
			peak = max(peak, size())
		}
	}
	t.Logf("steady-state database: peak %d bytes (%.1f MiB) at %d rows, %d answer bytes",
		peak, float64(peak)/(1<<20), idempotencyMaxRows, idempotencyMaxBodyBytes)
	// idempotencyMaxBodyBytes' comment quotes this measurement (5.1 MiB).
	if limit := int64(6 << 20); peak > limit {
		t.Fatalf("worst-case database %d bytes exceeds the documented bound %d", peak, limit)
	}
}

func TestFingerprintKeyCollisions(t *testing.T) {
	same := func(a, b string) bool { return bodyFingerprint([]byte(a)) == bodyFingerprint([]byte(b)) }
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{`{"op":"restart","trigger_key":"ansible"}`, `{ "trigger_key":"ansible", "op":"restart" }`, true},
		{`{"op":"down","OP":"up"}`, `{"OP":"up","op":"down"}`, false},
		{`{"op":"up","op":"down"}`, `{"op":"down","op":"up"}`, false},
		{`{"a":{"x":1,"X":2}}`, `{"a":{"X":2,"x":1}}`, false},
		{`{"ids":[{"k":1},{"K":2}]}`, `{"ids":[{"K":2},{"k":1}]}`, false}, // no collision within one object
		{`{"a":1,"b":{"a":2}}`, `{"b":{"a":2},"a":1}`, true},              // same key in different objects is fine
	} {
		if got := same(c.a, c.b); got != c.want {
			t.Errorf("same(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if !jsonKeysCollide([]byte(`{"x":[1,{"y":2,"Y":3}]}`)) || jsonKeysCollide([]byte(`[{"a":1},{"A":2}]`)) {
		t.Fatal("collision detection is wrong on nesting")
	}

	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	e.writeCompose(t, "owned", "services: {}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned")}))
	if r := e.doKeyed(t, "POST", "/v1/projects/owned/op", "k-case", `{"op":"restart","OP":"down"}`); r.status != 202 {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	if r := e.doKeyed(t, "POST", "/v1/projects/owned/op", "k-case", `{"OP":"down","op":"restart"}`); r.status != 422 || r.code() != "idempotency_key_reused" {
		t.Fatalf("a body that binds to a different op must not replay: %d %s", r.status, r.body)
	}
	e.waitJobs(t)
}

func TestIdempotencyAgeExemptsThisProcessUntilItsUptimePassesTheWindow(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	db, store := e.a.events.DB(), e.a.idem
	boot := time.Unix(86_400, 0) // a Pi with no RTC boots in 1970
	now := boot
	store.now = func() time.Time { return now }
	uptime := time.Minute
	store.uptime = func() time.Duration { return uptime }

	if r := e.doKeyed(t, "POST", "/v1/containers/gdrive-agent/stop", "k-mine", ""); r.status != 202 {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	_, err := db.Exec(`INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, status, body, created_at_ns, proc)
		VALUES ('POST', 'x', 'k-theirs', 'fp', 'done', 202, '{}', ?, 'previous-process')`, boot.UnixNano())
	must(t, err)

	now = boot.Add(50 * 365 * 24 * time.Hour) // NTP corrects the clock
	_, err = store.prune(context.Background())
	must(t, err)
	if idemRows(t, db, "key = 'k-mine'") != 1 {
		t.Fatal("an answer this process recorded minutes ago was aged out by a clock jump")
	}
	if idemRows(t, db, "key = 'k-theirs'") != 0 {
		t.Fatal("another process's old answer must still age out")
	}
	uptime = idempotencyWindow + time.Minute
	_, err = store.prune(context.Background())
	must(t, err)
	if idemRows(t, db, "key = 'k-mine'") != 0 {
		t.Fatal("once the process has been up longer than the window, its old rows age out too")
	}
	e.waitJobs(t)
}

// TestIdempotencyPrunesWithoutARestart: a recorded answer runs the prune once the
// interval has passed, so a long-lived agent stays bounded.
func TestIdempotencyPrunesWithoutARestart(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	db, store := e.a.events.DB(), e.a.idem
	now := time.Now()
	store.now = func() time.Time { return now }
	store.uptime = func() time.Duration { return 2 * idempotencyWindow }
	_, err := db.Exec(`INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, status, body, created_at_ns, proc)
		VALUES ('POST', 'x', 'k-old', 'fp', 'done', 202, '{}', ?, 'previous-process')`, now.Add(-idempotencyWindow-time.Hour).UnixNano())
	must(t, err)

	if r := e.doKeyed(t, "POST", "/v1/containers/gdrive-agent/stop", "k-a", ""); r.status != 202 {
		t.Fatalf("got %d", r.status)
	}
	if idemRows(t, db, "key = 'k-old'") != 1 {
		t.Fatal("precondition: inside the prune interval nothing is pruned")
	}
	now = now.Add(idempotencyPruneEvery + time.Second)
	if r := e.doKeyed(t, "POST", "/v1/containers/gdrive-agent/restart", "k-b", ""); r.status != 202 {
		t.Fatalf("got %d", r.status)
	}
	if idemRows(t, db, "key = 'k-old'") != 0 {
		t.Fatal("a recorded answer past the interval must run the prune")
	}
	e.waitJobs(t)
}

// TestIdempotencyRecordsWhenTheClientHasGone: the request context is cancelled the
// moment the client disconnects; the answer must still be stored.
func TestIdempotencyRecordsWhenTheClientHasGone(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	r := gin.New()
	var cancel context.CancelFunc
	r.POST("/job", e.a.idempotent(), func(c *gin.Context) {
		c.JSON(http.StatusAccepted, gin.H{"job_id": "j-1"})
		cancel() // the client hangs up after the response started
	})
	ctx, cancelFn := context.WithCancel(context.Background())
	cancel = cancelFn
	req := httptest.NewRequest(http.MethodPost, "/job", nil).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "k-gone")
	r.ServeHTTP(httptest.NewRecorder(), req)
	if n := idemRows(t, e.a.events.DB(), "key = 'k-gone' AND state = 'done'"); n != 1 {
		t.Fatal("the answer was not recorded after the client disconnected")
	}
}

func TestIdempotencyRecordNeverOverwritesAnotherAnswer(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	s, db := e.a.idem, e.a.events.DB()
	scope := pathSHA("/v1/x")
	_, err := db.Exec(`INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, status, body, created_at_ns, proc)
		VALUES ('POST', ?, 'k', 'other-fp', 'done', 202, '{"job_id":"theirs"}', ?, ?)`, scope, time.Now().UnixNano(), s.proc)
	must(t, err)
	err = s.record(context.Background(), pendingAnswer{method: "POST", pathSHA: scope, key: "k", fp: "my-fp", status: 202, body: []byte(`{"job_id":"mine"}`)})
	if err != errClaimLost {
		t.Fatalf("got %v, want errClaimLost", err)
	}
	var body string
	must(t, db.QueryRow(`SELECT body FROM idempotency_keys WHERE key = 'k'`).Scan(&body))
	if body != `{"job_id":"theirs"}` {
		t.Fatalf("another request's answer was overwritten: %s", body)
	}
	// A vanished claim (swept or pruned) is written, not silently dropped.
	err = s.record(context.Background(), pendingAnswer{method: "POST", pathSHA: scope, key: "k2", fp: "fp", status: 202, body: []byte(`{}`)})
	if err != nil || idemRows(t, db, "key = 'k2' AND state = 'done'") != 1 {
		t.Fatalf("a vanished claim's answer must still be stored: %v", err)
	}
}
