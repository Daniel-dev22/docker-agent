package main

// Idempotency-Key for job-starting requests.
//
// A caller that loses the response — the agent restarted mid-request, a proxy
// timed out, the network dropped — cannot tell whether its request ran. Resending
// it blind can run a compose op or a container verb twice. With an
// `Idempotency-Key` header the agent records the ANSWER it gave, durably, and a
// resend with the same key returns that answer instead of acting again.
//
//   - Scope is (method, path, key); the path is stored as its SHA-256 so a row's
//     size does not depend on it. The request body is fingerprinted: the same key
//     with a different body is the caller's bug, 422 idempotency_key_reused.
//   - The request body is already bounded by boundedBody (bodylimit.go).
//   - The claim is taken atomically (INSERT … ON CONFLICT DO NOTHING) BEFORE the
//     handler runs, so a concurrent duplicate gets 409 idempotency_key_in_flight
//     and never executes.
//   - Deterministic answers (2xx, and 4xx refusals) are recorded. A 5xx — above all
//     503 self_identity_unavailable — is transient: the claim is released so a
//     retry can succeed later.
//   - An answer to a request that ACTED but could not be stored is kept in memory
//     and served to retries while a background writer keeps trying to store it;
//     close() makes one last bounded attempt at every such answer on shutdown.
//     Only an answer that still cannot be written then is lost — its claim stays in
//     flight, the boot sweep releases it, and a retry re-runs the request.
//   - Claims still in flight when the process died cannot have a recorded answer;
//     the boot sweep releases them before the listener binds. Any job such a
//     request had started was interrupted with the process and is failed by the
//     orphan sweep, so re-running on retry is the correct outcome.
//   - The table is bounded by time AND rows AND bytes (idempotencyStore.prune),
//     pruned at boot and at most once a minute after a recorded answer — never on a
//     ticker.
//
// Without the header a request behaves exactly as it always has.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	idempotencyHeader         = "Idempotency-Key"
	idempotencyReplayedHeader = "Idempotency-Replayed"
	idempotencyMaxKeyLen      = 128

	// idempotencyWindow: a key is honoured for 24h. A retry exists to recover from
	// a lost response within one operator action or one playbook run; a day
	// covers both with room to spare.
	idempotencyWindow = 24 * time.Hour

	// idempotencyMaxRows caps the table whatever the rate. Measured (2026-09-17,
	// modernc sqlite, WAL checkpointed): 1,000 rows of a 45-char key, a hashed op
	// path, a 64-hex fingerprint, a process id and a 413-byte 409 refusal body — a
	// typical large answer — grew the database by 856 B/row including both indexes.
	// Job-starting requests per host are an Ansible run plus operator clicks — an
	// estimate of a few hundred on a heavy day — so the cap binds only if something
	// replays fresh keys in a loop, and then the oldest answers go first.
	// TestIdempotencyRowSize keeps the per-row figure honest.
	idempotencyMaxRows = 5000

	// idempotencyMaxBodyBytes caps the recorded answers' bytes (bodies and keys),
	// independently of the row count: rows are only small because answers usually
	// are, and a refusal naming 100 targets is ~50 KB. 2 MiB is the row cap times
	// a typical 413-byte answer. With BOTH caps binding — 5,000 rows of 128-byte
	// keys and answers filling the 2 MiB — and the table turned over three times so
	// freed pages count, the whole database peaked at 5,369,856 bytes (5.1 MiB,
	// measured 2026-09-17); TestIdempotencyWorstCaseSize fails above 6 MiB.
	idempotencyMaxBodyBytes = 2 << 20

	// idempotencyPruneEvery bounds how often a recorded answer also runs the prune.
	idempotencyPruneEvery = time.Minute

	// idempotencyMaxPending bounds the in-memory answers awaiting a durable write.
	// Each exists only while sqlite is refusing writes; beyond the cap a retry
	// falls back to 409 idempotency_key_in_flight, which the client polls.
	idempotencyMaxPending = 1024
	idempotencyRetryMax   = time.Minute

	// idempotencyCloseBudget bounds close()'s final writes of pending answers,
	// all of them together. Shutdown is the HTTP server's 10s drain (main.go) plus
	// this; a sqlite write that has not landed in 2s during shutdown is not going
	// to, and the process must still exit.
	idempotencyCloseBudget = 2 * time.Second
)

// idempotencyStore persists claims and recorded answers in events.sqlite.
type idempotencyStore struct {
	db *sql.DB

	// proc identifies rows claimed by this process: the age prune exempts them
	// until the process has been up longer than the window, so a wall clock that
	// was wrong at boot (a Pi with no RTC) cannot age out answers it just recorded.
	proc    string
	started time.Time // carries the monotonic reading uptime is measured by

	now       func() time.Time
	uptime    func() time.Duration
	retryBase time.Duration

	lastPruned atomic.Int64 // unix ns

	mu      sync.Mutex
	pending map[string]pendingAnswer

	done    chan struct{}
	closing sync.Once
	writers sync.WaitGroup
}

type pendingAnswer struct {
	method, pathSHA, key, fp string
	status                   int
	body                     []byte
}

func newIdempotencyStore(db *sql.DB) (*idempotencyStore, error) {
	if db == nil {
		return nil, errors.New("idempotency store: db is required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS idempotency_keys (
			method        TEXT    NOT NULL,
			path_sha      TEXT    NOT NULL,
			key           TEXT    NOT NULL,
			fingerprint   TEXT    NOT NULL,
			state         TEXT    NOT NULL,  -- in_flight | done
			status        INTEGER NOT NULL DEFAULT 0,
			body          BLOB,
			created_at_ns INTEGER NOT NULL,
			proc          TEXT    NOT NULL,
			PRIMARY KEY (method, path_sha, key)
		);
		CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created ON idempotency_keys (created_at_ns);
	`); err != nil {
		return nil, fmt.Errorf("idempotency store: create schema: %w", err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("idempotency store: process id: %w", err)
	}
	s := &idempotencyStore{
		db:        db,
		proc:      hex.EncodeToString(id[:]),
		started:   time.Now(),
		now:       time.Now,
		retryBase: time.Second,
		pending:   map[string]pendingAnswer{},
		done:      make(chan struct{}),
	}
	s.uptime = func() time.Duration { return time.Since(s.started) }
	return s, nil
}

// close stops the background writers, waits for them, then makes one final
// attempt to store every answer still pending — all within idempotencyCloseBudget.
// An answer stored here is replayed after the restart instead of the request
// being re-run.
func (s *idempotencyStore) close() {
	if s == nil {
		return
	}
	s.closing.Do(func() { close(s.done) })
	s.writers.Wait()

	s.mu.Lock()
	pending := make([]pendingAnswer, 0, len(s.pending))
	for _, a := range s.pending {
		pending = append(pending, a)
	}
	s.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	// The sqlite driver does not abandon a lock wait when a context expires: the
	// wait is sqlite's own busy_timeout (5s on this database), so the bound is set
	// there. Every final write goes through one connection whose busy_timeout is the
	// budget, restored before the connection returns to the pool.
	deadline := time.Now().Add(idempotencyCloseBudget)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		slog.Error("idempotency answers lost at shutdown — no connection", "lost", len(pending), "error", err)
		return
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `PRAGMA busy_timeout = 5000`)
		_ = conn.Close()
	}()
	stored := 0
	for _, a := range pending {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, remaining.Milliseconds())); err != nil {
			break
		}
		if err := s.recordOn(ctx, conn, a); err != nil && !errors.Is(err, errClaimLost) {
			continue
		}
		stored++
		s.mu.Lock()
		delete(s.pending, scopeKey(a.method, a.pathSHA, a.key))
		s.mu.Unlock()
	}
	if stored < len(pending) {
		slog.Error("idempotency answers lost at shutdown — their requests re-run on retry",
			"lost", len(pending)-stored, "stored", stored)
	}
}

func pathSHA(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

func scopeKey(method, pathSHA, key string) string {
	return method + "\x00" + pathSHA + "\x00" + key
}

type idemOutcome int

const (
	idemClaimed idemOutcome = iota
	idemReplay
	idemInFlight
	idemReused
)

type idemRecord struct {
	status int
	body   []byte
}

// claim atomically claims (method, path, key) for this request, or reports what
// an earlier request with the same key left behind.
func (s *idempotencyStore) claim(ctx context.Context, method, pathSHA, key, fp string) (idemOutcome, idemRecord, error) {
	s.mu.Lock()
	p, ok := s.pending[scopeKey(method, pathSHA, key)]
	s.mu.Unlock()
	if ok {
		if p.fp != fp {
			return idemReused, idemRecord{}, nil
		}
		return idemReplay, idemRecord{status: p.status, body: p.body}, nil
	}
	// The existing row can be released between the insert and the read (its answer
	// was transient); then the key is free and the insert is retried — a bounded
	// number of times, since each retry needs another request to have raced it.
	for range 3 {
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, created_at_ns, proc)
			VALUES (?, ?, ?, ?, 'in_flight', ?, ?)
			ON CONFLICT (method, path_sha, key) DO NOTHING`,
			method, pathSHA, key, fp, s.now().UnixNano(), s.proc)
		if err != nil {
			return 0, idemRecord{}, fmt.Errorf("claim: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return idemClaimed, idemRecord{}, nil
		}
		var (
			storedFP, state string
			rec             idemRecord
		)
		err = s.db.QueryRowContext(ctx, `
			SELECT fingerprint, state, status, body FROM idempotency_keys
			WHERE method = ? AND path_sha = ? AND key = ?`, method, pathSHA, key).
			Scan(&storedFP, &state, &rec.status, &rec.body)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, idemRecord{}, fmt.Errorf("read claim: %w", err)
		}
		switch {
		case storedFP != fp:
			return idemReused, idemRecord{}, nil
		case state == "done":
			return idemReplay, rec, nil
		default:
			return idemInFlight, idemRecord{}, nil
		}
	}
	return idemInFlight, idemRecord{}, nil
}

// errClaimLost: the answer could not be stored because the key now belongs to a
// different request (the claim was swept or pruned and re-claimed).
var errClaimLost = errors.New("idempotency claim lost to a different request")

// record stores the answer to a claimed request. It writes the row whether the
// claim is still in flight or has vanished (swept, pruned) — but never over an
// answer another request recorded.
func (s *idempotencyStore) record(ctx context.Context, a pendingAnswer) error {
	return s.recordOn(ctx, s.db, a)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *idempotencyStore) recordOn(ctx context.Context, db execer, a pendingAnswer) error {
	res, err := db.ExecContext(ctx, `
		INSERT INTO idempotency_keys (method, path_sha, key, fingerprint, state, status, body, created_at_ns, proc)
		VALUES (?, ?, ?, ?, 'done', ?, ?, ?, ?)
		ON CONFLICT (method, path_sha, key) DO UPDATE SET state = 'done', status = excluded.status, body = excluded.body
		WHERE idempotency_keys.state = 'in_flight' AND idempotency_keys.fingerprint = excluded.fingerprint`,
		a.method, a.pathSHA, a.key, a.fp, a.status, a.body, s.now().UnixNano(), s.proc)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return errClaimLost
	}
	return nil
}

// release drops a claim whose answer was transient, so a retry runs again.
func (s *idempotencyStore) release(ctx context.Context, method, pathSHA, key string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE method = ? AND path_sha = ? AND key = ? AND state = 'in_flight'`,
		method, pathSHA, key)
	return err
}

// keepPending serves an answer that could not be stored from memory, and keeps
// trying to store it in the background until it lands or the store closes.
func (s *idempotencyStore) keepPending(a pendingAnswer) bool {
	sk := scopeKey(a.method, a.pathSHA, a.key)
	s.mu.Lock()
	if len(s.pending) >= idempotencyMaxPending {
		s.mu.Unlock()
		return false
	}
	s.pending[sk] = a
	s.mu.Unlock()

	s.writers.Add(1)
	go func() {
		defer s.writers.Done()
		wait := s.retryBase
		for {
			select {
			case <-s.done:
				return // close() makes the final attempt
			case <-time.After(wait):
			}
			err := s.record(context.Background(), a)
			if err == nil || errors.Is(err, errClaimLost) {
				if err != nil {
					slog.Warn("idempotency answer could not be stored: key re-claimed by another request", "key", a.key)
				}
				s.mu.Lock()
				delete(s.pending, sk)
				s.mu.Unlock()
				return
			}
			slog.Warn("idempotency answer still not stored — retrying", "error", err, "retry_in", min(wait*2, idempotencyRetryMax))
			wait = min(wait*2, idempotencyRetryMax)
		}
	}()
	return true
}

// sweepInFlight releases every in-flight claim. Called once at boot, before the
// listener binds, so no claim of the running process can be among them.
func (s *idempotencyStore) sweepInFlight(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE state = 'in_flight'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// prune enforces all three bounds.
//
//   - Age: rows created before the window — except rows this process created,
//     until it has been up longer than the window (a wrong boot clock).
//   - Rows: the newest idempotencyMaxRows recorded answers are kept.
//   - Bytes: the newest recorded answers whose bodies and keys fit
//     idempotencyMaxBodyBytes are kept.
//
// Size eviction is oldest first by insertion order (rowid), which no clock can
// reorder, and never touches an in-flight claim.
func (s *idempotencyStore) prune(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-idempotencyWindow).UnixNano()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE created_at_ns < ? AND (proc <> ? OR ?)`,
		cutoff, s.proc, s.uptime() > idempotencyWindow)
	if err != nil {
		return 0, fmt.Errorf("prune by age: %w", err)
	}
	byAge, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE rowid IN (
			SELECT rowid FROM idempotency_keys WHERE state = 'done'
			ORDER BY rowid DESC LIMIT -1 OFFSET ?)`, idempotencyMaxRows)
	if err != nil {
		return byAge, fmt.Errorf("prune by rows: %w", err)
	}
	byRows, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE rowid IN (
			SELECT rowid FROM (
				SELECT rowid, SUM(LENGTH(COALESCE(body, '')) + LENGTH(key)) OVER (ORDER BY rowid DESC) AS kept
				FROM idempotency_keys WHERE state = 'done')
			WHERE kept > ?)`, idempotencyMaxBodyBytes)
	if err != nil {
		return byAge + byRows, fmt.Errorf("prune by bytes: %w", err)
	}
	byBytes, _ := res.RowsAffected()
	return byAge + byRows + byBytes, nil
}

// maybePrune runs prune at most once per idempotencyPruneEvery.
func (s *idempotencyStore) maybePrune(ctx context.Context) {
	now := s.now().UnixNano()
	last := s.lastPruned.Load()
	if now-last < int64(idempotencyPruneEvery) || !s.lastPruned.CompareAndSwap(last, now) {
		return
	}
	if n, err := s.prune(ctx); err != nil {
		slog.Warn("idempotency prune failed", "error", err)
	} else if n > 0 {
		slog.Info("idempotency keys pruned", "rows", n)
	}
}

// validIdempotencyKey: 1–128 printable ASCII characters.
func validIdempotencyKey(k string) bool {
	if k == "" || len(k) > idempotencyMaxKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// bodyFingerprint hashes the request body in a canonical form, so a client that
// re-serialises the same JSON with different key order or whitespace still
// matches. Numbers keep their exact text (UseNumber). A body that is not JSON — or
// whose meaning depends on key ORDER — is hashed as bytes.
//
// That last case is real: encoding/json binds object keys case-insensitively and
// the last one wins, so {"op":"down","OP":"up"} runs `up` and {"OP":"up","op":"down"}
// runs `down`. A canonical form sorts both to the same bytes; the raw bytes differ.
func bodyFingerprint(raw []byte) string {
	canon := bytes.TrimSpace(raw)
	if len(canon) > 0 && !jsonKeysCollide(canon) {
		dec := json.NewDecoder(bytes.NewReader(canon))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err == nil && !dec.More() {
			if b, err := json.Marshal(v); err == nil {
				canon = b
			}
		}
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// jsonKeysCollide reports whether any object in raw repeats a key under the
// folding encoding/json binds with. Malformed JSON reports false (it is hashed as
// bytes anyway).
func jsonKeysCollide(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type frame struct {
		object    bool
		expectKey bool
		keys      map[string]struct{}
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.expectKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = true
				}
				continue
			}
			k := foldJSONKey(tok.(string))
			if _, dup := top.keys[k]; dup {
				return true
			}
			top.keys[k] = struct{}{}
			top.expectKey = false
			continue
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{':
				stack = append(stack, &frame{object: true, expectKey: true, keys: map[string]struct{}{}})
				continue
			case '[':
				stack = append(stack, &frame{})
				continue
			default: // ']' — '}' is handled above
				stack = stack[:len(stack)-1]
			}
		}
		// A complete value ended: the enclosing object expects its next key.
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
		if len(stack) == 0 && !dec.More() {
			return false
		}
	}
}

// foldJSONKey folds a key exactly as encoding/json matches object keys to struct
// fields (encoding/json/fold.go): ASCII upper-cased, every other rune mapped to the
// smallest rune of its unicode.SimpleFold orbit. strings.ToLower is not that
// relation — "ſ" (U+017F) and the Kelvin sign (U+212A) match "s" and "k" there.
func foldJSONKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for _, r := range k {
		if r < utf8.RuneSelf {
			if 'a' <= r && r <= 'z' {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			continue
		}
		for {
			r2 := unicode.SimpleFold(r)
			if r2 <= r {
				r = r2
				break
			}
			r = r2
		}
		b.WriteRune(r)
	}
	return b.String()
}

// captureWriter keeps a copy of the response body the handler writes.
type captureWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// idempotent wraps a job-starting route.
func (a *app) idempotent() gin.HandlerFunc {
	return func(c *gin.Context) {
		key, present := c.Request.Header[http.CanonicalHeaderKey(idempotencyHeader)]
		if !present {
			c.Next()
			return
		}
		if len(key) != 1 || !validIdempotencyKey(key[0]) {
			refuse(c, http.StatusBadRequest, "invalid_idempotency_key",
				fmt.Sprintf("%s must be one header of 1-%d printable ASCII characters", idempotencyHeader, idempotencyMaxKeyLen), nil)
			return
		}
		if a.idem == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "idempotency store not configured"})
			return
		}
		// boundedBody has already read at most maxRequestBodyBytes into memory.
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			// An I/O failure, not a malformed body: transient, so no code.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "read request body: " + echo(err.Error())})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		method, scope, k := c.Request.Method, pathSHA(c.Request.URL.Path), key[0]
		fp := bodyFingerprint(raw)
		ctx := c.Request.Context()

		outcome, rec, err := a.idem.claim(ctx, method, scope, k, fp)
		if err != nil {
			slog.Error("idempotency claim failed", "error", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "idempotency store unavailable: " + echo(err.Error())})
			return
		}
		switch outcome {
		case idemReplay:
			c.Header(idempotencyReplayedHeader, "true")
			c.Data(rec.status, "application/json; charset=utf-8", rec.body)
			c.Abort()
			return
		case idemReused:
			refuse(c, http.StatusUnprocessableEntity, "idempotency_key_reused",
				idempotencyHeader+" was already used for a different request body on this endpoint", nil)
			return
		case idemInFlight:
			// The one refusal that is NOT final: the answer is coming. See retryableCodes.
			refuse(c, http.StatusConflict, "idempotency_key_in_flight",
				"a request with this "+idempotencyHeader+" is still being handled; retry for its answer",
				gin.H{"retryable": true})
			return
		}

		// Claimed: run the handler, then record or release. The deferred release also
		// covers a handler panic, so a claim can never be stranded in flight.
		cw := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = cw
		settled := false
		// The request context is cancelled the moment the client goes away; the
		// answer must be settled regardless.
		settleCtx := context.WithoutCancel(ctx)
		defer func() {
			if !settled {
				if err := a.idem.release(settleCtx, method, scope, k); err != nil {
					slog.Error("idempotency release failed", "error", err)
				}
			}
		}()
		c.Next()
		status := cw.Status()
		if status >= http.StatusInternalServerError {
			return // transient: the deferred release lets a retry run
		}
		answer := pendingAnswer{method: method, pathSHA: scope, key: k, fp: fp, status: status, body: bytes.Clone(cw.buf.Bytes())}
		if err := a.idem.record(settleCtx, answer); err != nil {
			switch {
			case errors.Is(err, errClaimLost):
				settled = true
				slog.Warn("idempotency answer not stored: key re-claimed by another request", "key", k)
			case status < http.StatusBadRequest:
				// The request ACTED and its answer could not be stored. Releasing would let
				// a retry act a second time: serve the answer from memory while it is
				// written in the background.
				settled = true
				if !a.idem.keepPending(answer) {
					slog.Error("idempotency answer not stored and the pending set is full — claim left in flight",
						"error", err)
				}
			default:
				slog.Error("idempotency record failed for a refusal — releasing the claim", "error", err)
			}
			return
		}
		settled = true
		a.idem.maybePrune(settleCtx)
	}
}

// sweepIdempotency is the boot half of the store's lifecycle: release claims left
// in flight by the previous process and enforce the bounds. It runs before the
// HTTP listener binds.
func (a *app) sweepIdempotency(ctx context.Context) {
	if a.idem == nil {
		return
	}
	if n, err := a.idem.sweepInFlight(ctx); err != nil {
		slog.Warn("idempotency boot sweep failed", "error", err)
	} else if n > 0 {
		slog.Warn("released idempotency claims left in flight by the previous process", "count", n)
	}
	if _, err := a.idem.prune(ctx); err != nil {
		slog.Warn("idempotency boot prune failed", "error", err)
	}
	a.idem.lastPruned.Store(a.idem.now().UnixNano())
}
