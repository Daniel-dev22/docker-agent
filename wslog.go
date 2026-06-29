package main

import (
	"bytes"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// streamLines is the shared WS line pump behind every text-log stream
// (job logs and container logs). Delivery is identical across them — only the
// SOURCE of the lines differs (an in-process job ring vs a demuxed Docker
// stream), so that uniqueness lives in the caller, not here.
//
// It writes the backlog (if any) then live lines from ch as newline-delimited
// TextMessage frames, runs a 20s keepalive ping, and spawns a reader goroutine
// to notice client disconnect. It returns when the client drops, ch closes, or
// a write fails. The caller owns conn.Close and the ch lifecycle.
//
// Perf: each live write COALESCES — it drains every line currently queued on ch
// into ONE frame (a single NextWriter + syscall) via a pooled buffer, so a burst
// of N lines costs one frame instead of N. The frontend splits frames on '\n',
// so batching is wire-compatible with line-per-frame.
func streamLines(conn *websocket.Conn, backlog []string, ch <-chan string) {
	// Reader goroutine: a failed read means the client is gone — closing the
	// conn here unblocks the writes below so the pump returns promptly.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				_ = conn.Close()
				return
			}
		}
	}()

	if len(backlog) > 0 && !writeFrame(conn, backlog) {
		return
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	batch := make([]string, 0, 64)
	for {
		select {
		case line, open := <-ch:
			if !open {
				return
			}
			var closed bool
			batch, closed = drainBatch(line, ch, batch)
			// Flush whatever we collected (incl. the pre-close tail), then stop
			// if the producer closed mid-drain or the write failed.
			if !writeFrame(conn, batch) || closed {
				return
			}
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		}
	}
}

// drainBatch resets batch and fills it with `first` plus every line currently
// queued on ch (non-blocking) — the coalescing core, so a burst of N queued
// lines becomes ONE frame. closed reports the producer closing ch mid-drain
// (the caller flushes the pre-close lines, then stops). Pure (no conn/time) so
// the batching is unit-testable without a socket.
func drainBatch(first string, ch <-chan string, batch []string) (out []string, closed bool) {
	batch = append(batch[:0], first)
	for {
		select {
		case l, open := <-ch:
			if !open {
				return batch, true
			}
			batch = append(batch, l)
		default:
			return batch, false
		}
	}
}

// wsFrameBufPool reuses the per-frame assembly buffer so a high-volume stream
// doesn't allocate a buffer per frame.
var wsFrameBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// writeFrame assembles lines into one newline-terminated TextMessage frame using
// a pooled buffer. Returns false on write error. All writes happen on the pump
// goroutine (ping included), so there are never concurrent writers on conn.
func writeFrame(conn *websocket.Conn, lines []string) bool {
	if len(lines) == 0 {
		return true
	}
	buf := wsFrameBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	for _, l := range lines {
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	w, err := conn.NextWriter(websocket.TextMessage)
	if err != nil {
		wsFrameBufPool.Put(buf)
		return false
	}
	_, werr := w.Write(buf.Bytes())
	cerr := w.Close()
	wsFrameBufPool.Put(buf)
	return werr == nil && cerr == nil
}
