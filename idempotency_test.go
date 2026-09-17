package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// withIdempotency gives an env the production store over a real events.sqlite in
// configDir.
func withIdempotency(t *testing.T, e *capEnv, configDir string) {
	t.Helper()
	cfg := e.a.cfg
	cfg.ConfigDir = configDir
	cfg.ControlCenterURL = "https://controller.example.invalid:1443"
	events, err := newEventBuffer(cfg, http.DefaultClient)
	must(t, err)
	t.Cleanup(events.close)
	store, err := newIdempotencyStore(events.DB())
	must(t, err)
	e.a.events = events
	e.a.idem = store
	e.a.sweepIdempotency(context.Background())
}

type rawResponse struct {
	status   int
	body     []byte
	replayed string
}

func (e *capEnv) doKeyed(t *testing.T, method, path, key string, body string) rawResponse {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return rawResponse{status: w.Code, body: w.Body.Bytes(), replayed: w.Header().Get("Idempotency-Replayed")}
}

func (r rawResponse) code() any {
	m := map[string]any{}
	_ = json.Unmarshal(r.body, &m)
	return m["code"]
}

func idemRows(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	must(t, db.QueryRow(`SELECT COUNT(*) FROM idempotency_keys WHERE `+where, args...).Scan(&n))
	return n
}

func TestIdempotencyReplaysTheOriginalAnswer(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())

	first := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-stop-1", "")
	if first.status != http.StatusAccepted || first.replayed != "" {
		t.Fatalf("first: %d %s", first.status, first.body)
	}
	e.waitJobs(t)
	again := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-stop-1", "")
	if again.status != first.status || !bytes.Equal(again.body, first.body) || again.replayed != "true" {
		t.Fatalf("resend: %d %s (replayed=%q), want %d %s", again.status, again.body, again.replayed, first.status, first.body)
	}
	e.waitJobs(t)
	if n := e.eng.mutationCount(); n != 1 {
		t.Fatalf("engine saw %d mutations, want 1", n)
	}
	if n := len(e.a.reg.list()); n != 1 {
		t.Fatalf("%d jobs, want 1", n)
	}

	t.Run("a-refusal-is-recorded-too", func(t *testing.T) {
		must(t, e.a.projects.register(ProjectEntry{Name: "outside", WorkingDir: "/srv/containers/outside"}))
		r1 := e.doKeyed(t, http.MethodPost, "/v1/projects/outside/op", "k-refused", `{"op":"update"}`)
		if r1.status != http.StatusConflict {
			t.Fatalf("got %d %s", r1.status, r1.body)
		}
		r2 := e.doKeyed(t, http.MethodPost, "/v1/projects/outside/op", "k-refused", `{"op":"update"}`)
		if r2.status != r1.status || !bytes.Equal(r2.body, r1.body) || r2.replayed != "true" {
			t.Fatalf("refusal resend: %d %s", r2.status, r2.body)
		}
	})

	t.Run("same-json-reserialised-is-the-same-request", func(t *testing.T) {
		e.writeCompose(t, "owned", "services: {}\n")
		must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned")}))
		r1 := e.doKeyed(t, http.MethodPost, "/v1/projects/owned/op", "k-canon", `{"op":"restart","trigger_key":"ansible"}`)
		r2 := e.doKeyed(t, http.MethodPost, "/v1/projects/owned/op", "k-canon", "{ \"trigger_key\" : \"ansible\",\n \"op\":\"restart\" }")
		if r1.status != http.StatusAccepted || r2.replayed != "true" || !bytes.Equal(r1.body, r2.body) {
			t.Fatalf("got %d %s / %d %s replayed=%q", r1.status, r1.body, r2.status, r2.body, r2.replayed)
		}
	})
}

func TestIdempotencyKeyReusedForADifferentBody(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	e.writeCompose(t, "owned", "services: {}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned")}))

	if r := e.doKeyed(t, http.MethodPost, "/v1/projects/owned/op", "k-reuse", `{"op":"restart"}`); r.status != http.StatusAccepted {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	r := e.doKeyed(t, http.MethodPost, "/v1/projects/owned/op", "k-reuse", `{"op":"down"}`)
	if r.status != http.StatusUnprocessableEntity || r.code() != "idempotency_key_reused" {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	e.waitJobs(t)
	if n := len(e.a.reg.list()); n != 1 {
		t.Fatalf("%d jobs, want 1", n)
	}
	// The same key on a DIFFERENT path is a different request.
	if r := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/restart", "k-reuse", ""); r.status != http.StatusAccepted {
		t.Fatalf("other path: got %d %s", r.status, r.body)
	}
}

func TestIdempotencyConcurrentDuplicateRunsOnce(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	hang := make(chan struct{})
	e.eng.set(func(f *fakeEngine) { f.listHang = hang })

	var first rawResponse
	var wg sync.WaitGroup
	wg.Go(func() { first = e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-race", "") })
	deadline := time.Now().Add(2 * time.Second)
	for idemRows(t, e.a.events.DB(), "key = ? AND state = 'in_flight'", "k-race") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first request never claimed the key")
		}
		time.Sleep(5 * time.Millisecond)
	}
	dup := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-race", "")
	if dup.status != http.StatusConflict || dup.code() != "idempotency_key_in_flight" {
		t.Fatalf("duplicate: %d %s", dup.status, dup.body)
	}
	close(hang)
	wg.Wait()
	if first.status != http.StatusAccepted {
		t.Fatalf("first: %d %s", first.status, first.body)
	}
	e.waitJobs(t)
	if n := e.eng.mutationCount(); n != 1 {
		t.Fatalf("engine saw %d mutations, want exactly 1", n)
	}
}

func TestIdempotencyTransientAnswerIsNotRecorded(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	e.eng.set(func(f *fakeEngine) { f.listErr = true })
	r := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-transient", "")
	if r.status != http.StatusServiceUnavailable || r.code() != "self_identity_unavailable" {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	if n := idemRows(t, e.a.events.DB(), "key = ?", "k-transient"); n != 0 {
		t.Fatalf("a transient answer left %d rows", n)
	}
	e.eng.set(func(f *fakeEngine) { f.listErr = false })
	r = e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-transient", "")
	if r.status != http.StatusAccepted || r.replayed != "" {
		t.Fatalf("retry after recovery: %d %s replayed=%q", r.status, r.body, r.replayed)
	}
	e.waitJobs(t)
	if n := e.eng.mutationCount(); n != 1 {
		t.Fatalf("engine saw %d mutations, want 1", n)
	}
}

func TestIdempotencySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	e1 := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e1, dir)
	first := e1.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/restart", "k-durable", "")
	if first.status != http.StatusAccepted {
		t.Fatalf("got %d %s", first.status, first.body)
	}
	e1.waitJobs(t)
	// A claim the "crashed" process never settled.
	_, err := e1.a.events.DB().Exec(`INSERT INTO idempotency_keys (method, path, key, fingerprint, state, created_at_ns)
		VALUES ('POST', '/v1/containers/gdrive-agent/stop', 'k-orphan', ?, 'in_flight', ?)`, bodyFingerprint(nil), time.Now().UnixNano())
	must(t, err)
	e1.a.events.close()

	e2 := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e2, dir) // runs the boot sweep
	again := e2.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/restart", "k-durable", "")
	if again.status != first.status || !bytes.Equal(again.body, first.body) || again.replayed != "true" {
		t.Fatalf("after restart: %d %s replayed=%q", again.status, again.body, again.replayed)
	}
	if n := e2.eng.mutationCount(); n != 0 {
		t.Fatalf("the new process acted %d times on a recorded request", n)
	}
	orphan := e2.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-orphan", "")
	if orphan.status != http.StatusAccepted || orphan.replayed != "" {
		t.Fatalf("a claim left in flight by the dead process must be released at boot: %d %s", orphan.status, orphan.body)
	}
}

func TestIdempotencyPruneBounds(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	db, store := e.a.events.DB(), e.a.idem
	now := time.Now()
	store.now = func() time.Time { return now }

	tx, err := db.Begin()
	must(t, err)
	insert := func(key, state string, at time.Time) {
		_, err := tx.Exec(`INSERT INTO idempotency_keys (method, path, key, fingerprint, state, status, body, created_at_ns)
			VALUES ('POST', '/v1/x', ?, 'fp', ?, 202, '{}', ?)`, key, state, at.UnixNano())
		must(t, err)
	}
	for i := range 10 {
		insert(fmt.Sprintf("old-%d", i), "done", now.Add(-idempotencyWindow-time.Minute))
	}
	for i := range idempotencyMaxRows + 50 {
		insert(fmt.Sprintf("recent-%05d", i), "done", now.Add(-time.Duration(idempotencyMaxRows+50-i)*time.Second))
	}
	insert("live-claim", "in_flight", now.Add(-time.Hour*23))
	must(t, tx.Commit())

	if _, err := store.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := idemRows(t, db, "key LIKE 'old-%'"); n != 0 {
		t.Fatalf("%d rows older than the window survived", n)
	}

	if n := idemRows(t, db, "state = 'done'"); n != idempotencyMaxRows {
		t.Fatalf("%d recorded rows after prune, want the cap %d", n, idempotencyMaxRows)
	}
	for _, gone := range []string{"recent-00000", "recent-00049"} {
		if idemRows(t, db, "key = ?", gone) != 0 {
			t.Fatalf("the oldest answers must go first; %s survived", gone)
		}
	}
	if idemRows(t, db, "key = 'recent-00050'") != 1 || idemRows(t, db, "key = 'live-claim'") != 1 {
		t.Fatal("prune removed a row it must keep")
	}

	// The age bound must hold on its own, far below the row cap — above, the cap
	// would have evicted the old rows anyway.
	t.Run("age-bound-below-the-cap", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM idempotency_keys`)
		must(t, err)
		tx, err := db.Begin()
		must(t, err)
		for i := range 5 {
			_, err := tx.Exec(`INSERT INTO idempotency_keys (method, path, key, fingerprint, state, status, body, created_at_ns)
				VALUES ('POST', '/v1/x', ?, 'fp', 'done', 202, '{}', ?)`, fmt.Sprintf("stale-%d", i), now.Add(-idempotencyWindow-time.Second).UnixNano())
			must(t, err)
			_, err = tx.Exec(`INSERT INTO idempotency_keys (method, path, key, fingerprint, state, status, body, created_at_ns)
				VALUES ('POST', '/v1/x', ?, 'fp', 'done', 202, '{}', ?)`, fmt.Sprintf("fresh-%d", i), now.Add(-idempotencyWindow+time.Minute).UnixNano())
			must(t, err)
		}
		must(t, tx.Commit())
		if _, err := store.prune(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := idemRows(t, db, "key LIKE 'stale-%'"); n != 0 {
			t.Fatalf("%d answers older than %v survived", n, idempotencyWindow)
		}
		if n := idemRows(t, db, "key LIKE 'fresh-%'"); n != 5 {
			t.Fatalf("answers inside the window were pruned: %d left", n)
		}
	})
}

func TestIdempotencyRowSize(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "events.sqlite")+"?_pragma=journal_mode(WAL)")
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
	before := size()
	body := []byte(`{"code":"project_not_operable","error":"compose files for duplicacy-agent-api are at /mnt/fast_storage/docker_container_volumes/duplicacy-agent-api, outside docker-agent's compose root (/mnt/fast_storage/docker_container_volumes/docker-agent/data): update loads the compose files, which the agent cannot see. Allowed: down, restart.","working_dir":"/mnt/fast_storage/docker_container_volumes/duplicacy-agent-api"}`)
	const rows = 1000
	for i := range rows {
		key := fmt.Sprintf("ansible-%s-%08d", strings.Repeat("f", 36), i)
		_, _, err := s.claim(context.Background(), "POST", "/v1/projects/duplicacy-agent-api/op", key, strings.Repeat("a", 64))
		must(t, err)
		must(t, s.record(context.Background(), "POST", "/v1/projects/duplicacy-agent-api/op", key, 409, body))
	}
	perRow := float64(size()-before) / rows
	t.Logf("%.0f bytes/row", perRow)
	// idempotencyMaxRows' comment sizes the table from this figure (815 B/row).
	if perRow > 1024 {
		t.Fatalf("%.0f bytes/row: re-derive idempotencyMaxRows", perRow)
	}
}

func TestIdempotencyHeaderValidationAndAbsence(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())

	for name, key := range map[string]string{
		"too-long":  strings.Repeat("k", idempotencyMaxKeyLen+1),
		"non-ascii": "clé",
		"control":   "a\x01b",
	} {
		t.Run(name, func(t *testing.T) {
			r := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", key, "")
			if r.status != http.StatusBadRequest || r.code() != "invalid_idempotency_key" {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		})
	}
	t.Run("empty-value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/containers/gdrive-agent/stop", nil)
		req.Header["Idempotency-Key"] = []string{""}
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_idempotency_key") {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("two-header-lines", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/containers/gdrive-agent/stop", nil)
		req.Header["Idempotency-Key"] = []string{"a", "b"}
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
	})
	if n := e.eng.mutationCount(); n != 0 {
		t.Fatalf("an invalid key acted %d times", n)
	}
	if r := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", strings.Repeat("k", idempotencyMaxKeyLen), ""); r.status != http.StatusAccepted {
		t.Fatalf("a 128-char key is valid: %d %s", r.status, r.body)
	}

	t.Run("absent-header-is-unchanged", func(t *testing.T) {
		before := idemRows(t, e.a.events.DB(), "1=1")
		for range 2 {
			if r := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/restart", "", ""); r.status != http.StatusAccepted || r.replayed != "" {
				t.Fatalf("got %d %s", r.status, r.body)
			}
		}
		if after := idemRows(t, e.a.events.DB(), "1=1"); after != before {
			t.Fatalf("requests without the header wrote %d rows", after-before)
		}
		e.waitJobs(t)
	})
}

// TestEveryJobStartingRouteIsIdempotent: a resend on each route replays.
func TestEveryJobStartingRouteIsIdempotent(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	e.writeCompose(t, "owned", "services: {}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned"), ComposeFiles: []string{"docker-compose.yml"}}))
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/containers/gdrive-agent/start", ""},
		{http.MethodPost, "/v1/containers/gdrive-agent/stop", ""},
		{http.MethodPost, "/v1/containers/gdrive-agent/restart", ""},
		{http.MethodDelete, "/v1/containers/gdrive-agent", ""},
		{http.MethodPost, "/v1/containers/bulk", `{"action":"restart","ids":["traefik"]}`},
		{http.MethodPost, "/v1/projects/owned/op", `{"op":"restart"}`},
		{http.MethodPost, "/v1/projects", `{"name":"fresh","files":{"docker-compose.yml":"services: {}\n"},"deploy":true}`},
		{http.MethodPost, "/v1/projects/owned/copy", `{"new_name":"owned-copy","deploy":true}`},
	} {
		t.Run(rq.method+rq.path, func(t *testing.T) {
			first := e.doKeyed(t, rq.method, rq.path, "route-key", rq.body)
			if first.status >= 500 {
				t.Fatalf("first: %d %s", first.status, first.body)
			}
			again := e.doKeyed(t, rq.method, rq.path, "route-key", rq.body)
			if again.replayed != "true" || !bytes.Equal(again.body, first.body) {
				t.Fatalf("not replayed: %d %s", again.status, again.body)
			}
		})
	}
}

func TestIdempotencyClaimSettledOnPanicAndRecordFailure(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	withIdempotency(t, e, t.TempDir())
	db := e.a.events.DB()

	t.Run("panic-releases", func(t *testing.T) {
		r := gin.New()
		r.Use(gin.Recovery())
		r.POST("/boom", e.a.idempotent(), func(*gin.Context) { panic("handler bug") })
		req := httptest.NewRequest(http.MethodPost, "/boom", nil)
		req.Header.Set("Idempotency-Key", "k-panic")
		r.ServeHTTP(httptest.NewRecorder(), req)
		if n := idemRows(t, db, "key = 'k-panic'"); n != 0 {
			t.Fatalf("a panicking handler stranded the claim (%d rows)", n)
		}
	})

	_, err := db.Exec(`CREATE TRIGGER fail_record BEFORE UPDATE ON idempotency_keys
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	must(t, err)
	t.Run("acted-but-unrecorded-stays-in-flight", func(t *testing.T) {
		r := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-acted", "")
		if r.status != http.StatusAccepted {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		dup := e.doKeyed(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", "k-acted", "")
		if dup.status != http.StatusConflict || dup.code() != "idempotency_key_in_flight" {
			t.Fatalf("a request that acted must not be re-run: %d %s", dup.status, dup.body)
		}
	})
	t.Run("refusal-unrecorded-is-released", func(t *testing.T) {
		must(t, e.a.projects.register(ProjectEntry{Name: "outside", WorkingDir: "/srv/containers/outside"}))
		r := e.doKeyed(t, http.MethodPost, "/v1/projects/outside/op", "k-refusal", `{"op":"update"}`)
		if r.status != http.StatusConflict {
			t.Fatalf("got %d %s", r.status, r.body)
		}
		if n := idemRows(t, db, "key = 'k-refusal'"); n != 0 {
			t.Fatalf("an unrecordable refusal must be released (%d rows)", n)
		}
	})
	e.waitJobs(t)
}
