package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
		{"cancel-unknown-job", "POST", "/v1/jobs/no-such-job/cancel", ``, 409, "job_not_cancellable"},
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
		big := `{"action":"stop","ids":["` + strings.Repeat("x", maxRequestBodyBytes) + `"]}`
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

// countingReader serves n bytes and records how many were read.
type countingReader struct {
	remaining, read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

// TestEveryBodyIsBounded: an un-keyed request is bounded too, and an oversized
// body is refused without reading past the limit.
func TestEveryBodyIsBounded(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	routes := []struct{ method, path string }{
		{"POST", "/v1/projects"},
		{"POST", "/v1/projects/owned/op"},
		{"POST", "/v1/projects/owned/copy"},
		{"POST", "/v1/containers/gdrive-agent/stop"},
		{"DELETE", "/v1/containers/gdrive-agent"},
		{"POST", "/v1/containers/bulk"},
		{"POST", "/v1/jobs/j/cancel"},
		{"POST", "/v1/images/refresh"},
	}
	for _, rt := range routes {
		t.Run("chunked"+rt.method+rt.path, func(t *testing.T) {
			body := &countingReader{remaining: 2 * maxRequestBodyBytes}
			req := httptest.NewRequest(rt.method, rt.path, body)
			req.ContentLength = -1 // undeclared: the limit must stop the read itself
			w := httptest.NewRecorder()
			e.r.ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), `"request_too_large"`) {
				t.Fatalf("got %d %s", w.Code, clipTo(w.Body.String(), 200))
			}
			if body.read > maxRequestBodyBytes+1 {
				t.Fatalf("read %d bytes of an oversized body; the limit is %d", body.read, maxRequestBodyBytes)
			}
		})
		t.Run("declared"+rt.method+rt.path, func(t *testing.T) {
			body := &countingReader{remaining: maxRequestBodyBytes + 1}
			req := httptest.NewRequest(rt.method, rt.path, body)
			req.ContentLength = maxRequestBodyBytes + 1
			w := httptest.NewRecorder()
			e.r.ServeHTTP(w, req)
			if w.Code != http.StatusRequestEntityTooLarge || body.read != 0 {
				t.Fatalf("got %d after reading %d bytes, want 413 before reading", w.Code, body.read)
			}
		})
	}
	// Raw bytes, not re-encoded JSON: an encoder compacts the padding away, and the
	// "at the limit" body would silently be a few dozen bytes.
	const payload = `{"action":"restart","ids":["gdrive-agent"]}`
	atLimit := payload + strings.Repeat(" ", maxRequestBodyBytes-len(payload))
	t.Run("at-the-limit-is-accepted", func(t *testing.T) {
		if len(atLimit) != maxRequestBodyBytes {
			t.Fatalf("precondition: body is %d bytes", len(atLimit))
		}
		if r := e.doKeyed(t, "POST", "/v1/containers/bulk", "", atLimit); r.status != http.StatusAccepted {
			t.Fatalf("a body of exactly the limit: got %d %s", r.status, clipTo(string(r.body), 200))
		}
	})
	t.Run("one-byte-over-is-refused", func(t *testing.T) {
		if r := e.doKeyed(t, "POST", "/v1/containers/bulk", "", atLimit+" "); r.status != http.StatusRequestEntityTooLarge || r.code() != "request_too_large" {
			t.Fatalf("limit+1: got %d %s", r.status, clipTo(string(r.body), 200))
		}
	})
	if n := e.eng.mutationCount(); n > 1 {
		t.Fatalf("oversized requests acted: %d mutations", n)
	}
	e.waitJobs(t)
}

// TestRetryableContract pins which refusals are not final. A coded 4xx is
// dead-lettered by consumers; only these codes may say "retry".
func TestRetryableContract(t *testing.T) {
	if want := map[string]bool{"idempotency_key_in_flight": true}; !reflect.DeepEqual(retryableCodes, want) {
		t.Fatalf("retryableCodes = %v: a new retryable 4xx is a contract change for every consumer", retryableCodes)
	}

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("marking a non-allowlisted code retryable must not be writable")
			}
		}()
		refuse(c, http.StatusConflict, "project_exists", "x", gin.H{"retryable": true})
	}()

	t.Run("in-flight-says-retryable", func(t *testing.T) {
		e := newCapEnv(t, testSelfID, defaultContainers)
		withIdempotency(t, e, t.TempDir())
		scope := pathSHA("/v1/containers/gdrive-agent/stop")
		_, _, err := e.a.idem.claim(context.Background(), "POST", scope, "k-busy", bodyFingerprint(nil))
		must(t, err)
		r := e.doKeyed(t, "POST", "/v1/containers/gdrive-agent/stop", "k-busy", "")
		var body map[string]any
		must(t, json.Unmarshal(r.body, &body))
		if r.status != http.StatusConflict || body["code"] != "idempotency_key_in_flight" || body["retryable"] != true {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
	t.Run("final-refusals-do-not", func(t *testing.T) {
		e := newCapEnv(t, testSelfID, defaultContainers)
		r := e.doKeyed(t, "POST", "/v1/projects/nope/op", "", `{"op":"up"}`)
		if r.status != 404 || strings.Contains(string(r.body), "retryable") {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
}

type errorRecorder struct{ msgs []string }

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// TestRefusalAuditCatches is the audit's negative control: an audit that never
// fires would leave every other refusal test vacuous.
func TestRefusalAuditCatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name   string
		status int
		body   string
		report bool
	}{
		{"coded-final", 409, `{"code":"project_exists","error":"x"}`, false},
		{"allowlisted-retryable", 409, `{"code":"idempotency_key_in_flight","retryable":true}`, false},
		{"code-less-4xx", 400, `{"error":"x"}`, true},
		{"final-code-marked-retryable", 409, `{"code":"project_exists","retryable":true}`, true},
		{"allowlisted-code-not-marked", 409, `{"code":"idempotency_key_in_flight"}`, true},
		{"5xx-ignored", 500, `{"error":"x"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &errorRecorder{}
			r := gin.New()
			r.Use(auditRefusalCodes(rec))
			r.POST("/v1/projects", func(g *gin.Context) { g.Data(c.status, "application/json", []byte(c.body)) })
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/projects", nil))
			if got := len(rec.msgs) > 0; got != c.report {
				t.Fatalf("audit reported %v (%v), want %v", got, rec.msgs, c.report)
			}
		})
	}
}

// failingReader returns some bytes and then an I/O error.
type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, `{"op":`), nil
	}
	return 0, errors.New("connection reset by peer")
}

// TestBodyReadFailureIsTransient: a body that cannot be READ is an I/O fault —
// a code-less 500 — while a malformed body stays a final 400 invalid_body.
func TestBodyReadFailureIsTransient(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	check := func(t *testing.T, h http.Handler) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/projects/owned/op", &failingReader{})
		req.ContentLength = -1
		req.Header.Set("Idempotency-Key", "k-io")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"code"`) {
			t.Fatalf("got %d %s, want a code-less 500", w.Code, w.Body.String())
		}
	}
	t.Run("body-limit-middleware", func(t *testing.T) { check(t, e.r) })
	t.Run("idempotency-middleware-alone", func(t *testing.T) {
		withIdempotency(t, e, t.TempDir())
		r := gin.New()
		r.POST("/v1/projects/:name/op", e.a.idempotent(), func(c *gin.Context) { c.Status(http.StatusAccepted) })
		check(t, r)
		if n := idemRows(t, e.a.events.DB(), "key = 'k-io'"); n != 0 {
			t.Fatalf("a failed read was claimed (%d rows)", n)
		}
	})
	t.Run("malformed-body-stays-final", func(t *testing.T) {
		r := e.doKeyed(t, "POST", "/v1/containers/bulk", "", `{"ids":`)
		if r.status != 400 || r.code() != "invalid_body" {
			t.Fatalf("got %d %s", r.status, r.body)
		}
	})
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

// TestRefuseBoundsItsMessage: the one refusal writer bounds the whole message,
// whatever a caller built it from.
func TestRefuseBoundsItsMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	refuse(c, http.StatusBadRequest, "invalid_body", strings.Repeat("m", 100_000), gin.H{"targets": []string{strings.Repeat("t", 100_000)}})
	if w.Code != http.StatusBadRequest || w.Body.Len() > refusalMessageMax+echoMax+200 {
		t.Fatalf("a %d-byte refusal", w.Body.Len())
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a refusal without a code must not be writable")
		}
	}()
	refuse(c, http.StatusBadRequest, "", "no code", nil)
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
	// encoding/json folds beyond ASCII: U+017F LATIN SMALL LETTER LONG S matches
	// "s", and U+212A KELVIN SIGN matches "k". Each collision below binds to one
	// struct field, so the body's meaning depends on key order.
	for _, c := range []struct{ a, b string }{
		{`{"ids":["x"],"idſ":["y"]}`, `{"idſ":["y"],"ids":["x"]}`},
		{`{"KEY":1,"KEY":2}`, `{"KEY":2,"KEY":1}`},
		{`{"ſ":1,"S":2}`, `{"S":2,"ſ":1}`},
	} {
		if same(c.a, c.b) {
			t.Errorf("same(%s, %s): a Unicode-folded collision must hash raw bytes", c.a, c.b)
		}
	}
	var probe struct {
		IDs []string `json:"ids"`
		Key int      `json:"key"`
	}
	must(t, json.Unmarshal([]byte(`{"ids":["x"],"idſ":["y"],"key":1,"Key":2}`), &probe))
	if probe.IDs[0] != "y" || probe.Key != 2 {
		t.Fatalf("precondition: encoding/json folds these keys onto one field: %+v", probe)
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
