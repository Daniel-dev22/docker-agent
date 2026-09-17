package main

// Idempotency-Key for job-starting requests.
//
// A caller that loses the response — the agent restarted mid-request, a proxy
// timed out, the network dropped — cannot tell whether its request ran. Resending
// it blind can run a compose op or a container verb twice. With an
// `Idempotency-Key` header the agent records the ANSWER it gave, durably, and a
// resend with the same key returns that answer instead of acting again.
//
//   - Scope is (method, path, key). The request body is fingerprinted: the same
//     key with a different body is the caller's bug, 422 idempotency_key_reused.
//   - The claim is taken atomically (INSERT … ON CONFLICT DO NOTHING) BEFORE the
//     handler runs, so a concurrent duplicate gets 409 idempotency_key_in_flight
//     and never executes.
//   - Deterministic answers (2xx, and 4xx refusals) are recorded. A 5xx — above all
//     503 self_identity_unavailable — is transient: the claim is released so a
//     retry can succeed later.
//   - Claims still in flight when the process died cannot have a recorded answer;
//     the boot sweep releases them before the listener binds. Any job such a
//     request had started was interrupted with the process and is failed by the
//     orphan sweep, so re-running on retry is the correct outcome.
//   - The table is bounded by time AND rows (idempotencyStore.prune), pruned at
//     boot and at most once a minute after a recorded answer — never on a ticker.
//
// Without the header a request behaves exactly as it always has.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

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
	// modernc sqlite, WAL checkpointed): 1,000 rows of a 45-char key, an op path, a
	// 64-hex fingerprint and a 413-byte 409 refusal body — the largest answer the
	// agent gives — grew the database by 815,104 bytes, 815 B/row including both
	// indexes. 5,000 rows is ~4.1 MB on a Pi's SD card. The busiest host runs 15
	// containers; job-starting requests per host are an Ansible run plus operator
	// clicks — an estimate of a few hundred on a heavy day — so the cap binds only
	// if something replays fresh keys in a loop, and then the oldest answers go
	// first. TestIdempotencyRowSize keeps the per-row figure honest.
	idempotencyMaxRows = 5000

	// idempotencyPruneEvery bounds how often a recorded answer also runs the prune:
	// pruning is write-triggered, not a ticker, so an idle agent does no work.
	idempotencyPruneEvery = time.Minute
)

// idempotencyStore persists claims and recorded answers in events.sqlite.
type idempotencyStore struct {
	db         *sql.DB
	lastPruned atomic.Int64 // unix ns
	now        func() time.Time
}

func newIdempotencyStore(db *sql.DB) (*idempotencyStore, error) {
	if db == nil {
		return nil, errors.New("idempotency store: db is required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS idempotency_keys (
			method        TEXT    NOT NULL,
			path          TEXT    NOT NULL,
			key           TEXT    NOT NULL,
			fingerprint   TEXT    NOT NULL,
			state         TEXT    NOT NULL,  -- in_flight | done
			status        INTEGER NOT NULL DEFAULT 0,
			body          BLOB,
			created_at_ns INTEGER NOT NULL,
			PRIMARY KEY (method, path, key)
		);
		CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created ON idempotency_keys (created_at_ns);
	`); err != nil {
		return nil, fmt.Errorf("idempotency store: create schema: %w", err)
	}
	return &idempotencyStore{db: db, now: time.Now}, nil
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
func (s *idempotencyStore) claim(ctx context.Context, method, path, key, fp string) (idemOutcome, idemRecord, error) {
	// The existing row can be released between the insert and the read (its answer
	// was transient); then the key is free and the insert is retried — a bounded
	// number of times, since each retry needs another request to have raced it.
	for range 3 {
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO idempotency_keys (method, path, key, fingerprint, state, created_at_ns)
			VALUES (?, ?, ?, ?, 'in_flight', ?)
			ON CONFLICT (method, path, key) DO NOTHING`,
			method, path, key, fp, s.now().UnixNano())
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
			WHERE method = ? AND path = ? AND key = ?`, method, path, key).
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

// record stores the answer to a claimed request.
func (s *idempotencyStore) record(ctx context.Context, method, path, key string, status int, body []byte) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE idempotency_keys SET state = 'done', status = ?, body = ?
		WHERE method = ? AND path = ? AND key = ? AND state = 'in_flight'`,
		status, body, method, path, key)
	return err
}

// release drops a claim whose answer was transient, so a retry runs again.
func (s *idempotencyStore) release(ctx context.Context, method, path, key string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE method = ? AND path = ? AND key = ? AND state = 'in_flight'`,
		method, path, key)
	return err
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

// prune enforces both bounds: recorded answers older than the window, then the
// oldest recorded answers beyond the row cap. In-flight claims are never pruned by
// the cap — they belong to requests running now — and a claim older than the
// window cannot be live.
func (s *idempotencyStore) prune(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-idempotencyWindow).UnixNano()
	res, err := s.db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at_ns < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune by age: %w", err)
	}
	byAge, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys WHERE rowid IN (
			SELECT rowid FROM idempotency_keys WHERE state = 'done'
			ORDER BY created_at_ns DESC LIMIT -1 OFFSET ?)`, idempotencyMaxRows)
	if err != nil {
		return byAge, fmt.Errorf("prune by rows: %w", err)
	}
	byRows, _ := res.RowsAffected()
	return byAge + byRows, nil
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
// matches. Numbers keep their exact text (UseNumber). A body that is not JSON is
// hashed as bytes.
func bodyFingerprint(raw []byte) string {
	canon := bytes.TrimSpace(raw)
	if len(canon) > 0 {
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
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("%s must be one header of 1-%d printable ASCII characters", idempotencyHeader, idempotencyMaxKeyLen),
				"code":  "invalid_idempotency_key",
			})
			return
		}
		if a.idem == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "idempotency store not configured"})
			return
		}
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "read request body: " + err.Error()})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		method, path, k := c.Request.Method, c.Request.URL.Path, key[0]
		ctx := c.Request.Context()

		outcome, rec, err := a.idem.claim(ctx, method, path, k, bodyFingerprint(raw))
		if err != nil {
			slog.Error("idempotency claim failed", "error", err, "path", path)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "idempotency store unavailable: " + err.Error()})
			return
		}
		switch outcome {
		case idemReplay:
			c.Header(idempotencyReplayedHeader, "true")
			c.Data(rec.status, "application/json; charset=utf-8", rec.body)
			c.Abort()
			return
		case idemReused:
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{
				"error": idempotencyHeader + " was already used for a different request body on this endpoint",
				"code":  "idempotency_key_reused",
			})
			return
		case idemInFlight:
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{
				"error": "a request with this " + idempotencyHeader + " is still being handled; retry for its answer",
				"code":  "idempotency_key_in_flight",
			})
			return
		}

		// Claimed: run the handler, then record or release. The deferred release also
		// covers a handler panic, so a claim can never be stranded in flight.
		cw := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = cw
		settled := false
		// The request context may already be cancelled once the handler returns;
		// settling the claim must not depend on it.
		settleCtx := context.WithoutCancel(ctx)
		defer func() {
			if !settled {
				if err := a.idem.release(settleCtx, method, path, k); err != nil {
					slog.Error("idempotency release failed", "error", err, "path", path)
				}
			}
		}()
		c.Next()
		status := cw.Status()
		if status >= http.StatusInternalServerError {
			return // transient: the deferred release lets a retry run
		}
		if err := a.idem.record(settleCtx, method, path, k, status, cw.buf.Bytes()); err != nil {
			if status < http.StatusBadRequest {
				// The request ACTED and its answer could not be stored. Releasing would let
				// a retry act a second time; leave the claim in flight instead — a retry
				// is told so, and the boot sweep frees it.
				settled = true
				slog.Error("idempotency record failed after the request acted — claim left in flight", "error", err, "path", path)
				return
			}
			slog.Error("idempotency record failed for a refusal — releasing the claim", "error", err, "path", path)
			return
		}
		settled = true
		a.idem.maybePrune(settleCtx)
	}
}

// sweepIdempotency is the boot half of the store's lifecycle: release claims left
// in flight by the previous process and enforce both bounds. It runs before the
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
