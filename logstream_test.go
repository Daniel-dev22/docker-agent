package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- lineWriter: the Docker-stream demux line splitter ---

func TestLineWriter(t *testing.T) {
	tests := []struct {
		name   string
		writes []string // each Write() call
		flush  bool
		want   []string
	}{
		{"single line", []string{"hello\n"}, false, []string{"hello"}},
		{"two lines one write", []string{"a\nb\n"}, false, []string{"a", "b"}},
		{"line split across writes", []string{"hel", "lo\n"}, false, []string{"hello"}},
		{"partial held until newline", []string{"no-newline-yet"}, false, nil},
		{"partial flushed", []string{"trailing"}, true, []string{"trailing"}},
		{"blank lines preserved", []string{"a\n\n\nb\n"}, false, []string{"a", "", "", "b"}},
		{"crlf kept (only LF splits)", []string{"a\r\nb\n"}, false, []string{"a\r", "b"}},
		{"newline boundary between writes", []string{"a", "\n", "b\n"}, false, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			lw := &lineWriter{emit: func(s string) { got = append(got, s) }}
			for _, w := range tt.writes {
				n, err := lw.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if tt.flush {
				lw.flush()
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A line longer than any plausible scanner cap must survive intact — lineWriter
// has no length limit (the reason we don't use bufio.Scanner).
func TestLineWriterLongLine(t *testing.T) {
	long := strings.Repeat("x", 2<<20) // 2 MiB
	var got []string
	lw := &lineWriter{emit: func(s string) { got = append(got, s) }}
	// Feed it in small chunks to exercise the cross-write buffer growth.
	data := []byte(long + "\n")
	for i := 0; i < len(data); i += 4096 {
		end := i + 4096
		if end > len(data) {
			end = len(data)
		}
		_, _ = lw.Write(data[i:end])
	}
	if len(got) != 1 || len(got[0]) != len(long) {
		t.Fatalf("got %d lines, first len %d, want 1 line of %d", len(got), len(got[0]), len(long))
	}
}

// --- drainBatch: the coalescing core (a burst of N queued lines → one frame) ---

func TestDrainBatch(t *testing.T) {
	t.Run("drains everything queued into one batch", func(t *testing.T) {
		ch := make(chan string, 8)
		for _, l := range []string{"b", "c", "d"} { // queued behind `first`
			ch <- l
		}
		batch, closed := drainBatch("a", ch, nil)
		if closed {
			t.Fatal("closed should be false")
		}
		if strings.Join(batch, ",") != "a,b,c,d" {
			t.Fatalf("batch = %v", batch)
		}
		if len(ch) != 0 {
			t.Fatalf("channel not fully drained: %d left", len(ch))
		}
	})

	t.Run("empty queue yields just first", func(t *testing.T) {
		ch := make(chan string, 4)
		batch, closed := drainBatch("only", ch, nil)
		if closed || strings.Join(batch, ",") != "only" {
			t.Fatalf("batch=%v closed=%v", batch, closed)
		}
	})

	t.Run("reports producer close mid-drain after collecting tail", func(t *testing.T) {
		ch := make(chan string, 4)
		ch <- "b"
		close(ch)
		batch, closed := drainBatch("a", ch, nil)
		if !closed {
			t.Fatal("closed should be true")
		}
		if strings.Join(batch, ",") != "a,b" {
			t.Fatalf("pre-close tail lost: %v", batch)
		}
	})

	t.Run("reuses the backing array (batch[:0])", func(t *testing.T) {
		ch := make(chan string, 1)
		prev := make([]string, 0, 8)
		ch <- "y"
		batch, _ := drainBatch("x", ch, prev)
		if &batch[0] != &prev[:1][0] {
			t.Error("drainBatch should reuse the caller's batch backing array")
		}
	})
}

// --- streamLines: end-to-end delivery over a real websocket conn ---

func TestStreamLinesDeliversBacklogThenLive(t *testing.T) {
	lines := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := fleetUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		streamLines(conn, []string{"b1", "b2"}, lines)
	}))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	read := func() string {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(msg)
	}

	if got := read(); got != "b1\nb2\n" { // backlog coalesced into one frame
		t.Fatalf("backlog frame = %q, want %q", got, "b1\nb2\n")
	}
	lines <- "L1"
	if got := read(); got != "L1\n" {
		t.Fatalf("live frame = %q, want %q", got, "L1\n")
	}

	// Channel close ends the stream → server returns → client read errors.
	close(lines)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("expected the stream to close after the channel drained")
	}
}

func BenchmarkLineWriter(b *testing.B) {
	// 200 short lines per write — the common container-log shape.
	chunk := []byte(strings.Repeat("2026-06-29T12:00:00Z stdout a log line of moderate length\n", 200))
	lw := &lineWriter{emit: func(string) {}}
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = lw.Write(chunk)
	}
}
