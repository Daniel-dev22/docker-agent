package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newTestGithubClient builds a client with no controller wiring, so only the
// unauthenticated paths are exercised (auth=false ⇒ ensureToken is never called).
func newTestGithubClient() *githubClient {
	g := newGithubClient(nil, "", nil)
	g.api = &http.Client{Timeout: 5 * time.Second}
	return g
}

// shrinkBackoff makes the retry loop run in microseconds instead of seconds.
func shrinkBackoff(t *testing.T) {
	t.Helper()
	prev := fetchBackoffBase
	fetchBackoffBase = time.Millisecond
	t.Cleanup(func() { fetchBackoffBase = prev })
}

func TestParseRawGithubURL(t *testing.T) {
	tests := []struct {
		name            string
		url             string
		repo, ref, path string
		ok              bool
	}{
		{
			name: "immich default template",
			url:  "https://raw.githubusercontent.com/immich-app/immich/v3.0.1/docker/docker-compose.yml",
			repo: "immich-app/immich", ref: "v3.0.1", path: "docker/docker-compose.yml", ok: true,
		},
		{
			name: "deeply nested path",
			url:  "https://raw.githubusercontent.com/o/n/main/a/b/c/compose.yml",
			repo: "o/n", ref: "main", path: "a/b/c/compose.yml", ok: true,
		},
		{
			name: "ref with slash is not split further",
			url:  "https://raw.githubusercontent.com/o/n/v1.2.3/docker-compose.yml",
			repo: "o/n", ref: "v1.2.3", path: "docker-compose.yml", ok: true,
		},
		{name: "non-github host falls through to plain GET", url: "https://git.example.com/o/n/main/compose.yml"},
		{name: "github.com is not the raw host", url: "https://github.com/o/n/blob/main/compose.yml"},
		{name: "missing path segment", url: "https://raw.githubusercontent.com/o/n/main"},
		{name: "empty owner", url: "https://raw.githubusercontent.com//n/main/compose.yml"},
		{name: "garbage", url: "://not a url"},
		// A signed URL's credentials live in the query; rewriting would drop them.
		{name: "query string is left alone", url: "https://raw.githubusercontent.com/o/n/main/compose.yml?token=abc"},
		{name: "fragment is left alone", url: "https://raw.githubusercontent.com/o/n/main/compose.yml#frag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo, ref, path, ok := parseRawGithubURL(tc.url)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if repo != tc.repo || ref != tc.ref || path != tc.path {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)", repo, ref, path, tc.repo, tc.ref, tc.path)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-30 * time.Second).UTC().Format(http.TimeFormat)

	tests := []struct {
		name  string
		value string
		want  time.Duration // 0 means "expect zero"; >0 means "expect roughly this"
		fuzzy bool
	}{
		{name: "absent", value: "", want: 0},
		{name: "delta seconds", value: "5", want: 5 * time.Second},
		{name: "zero seconds", value: "0", want: 0},
		{name: "negative seconds", value: "-3", want: 0},
		{name: "garbage", value: "soon", want: 0},
		{name: "http-date in the future", value: future, want: 30 * time.Second, fuzzy: true},
		{name: "http-date in the past", value: past, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set("Retry-After", tc.value)
			}
			got := retryAfter(h)
			if tc.fuzzy {
				if got < tc.want-2*time.Second || got > tc.want+time.Second {
					t.Fatalf("retryAfter = %v, want ~%v", got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("retryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetBytesRetriesTransient is the regression guard for the immich 429: a
// bursty rate-limit must be ridden out, not surfaced as a hard failure.
func TestGetBytesRetriesTransient(t *testing.T) {
	shrinkBackoff(t)

	tests := []struct {
		name         string
		statuses     []int // status per attempt; the last one repeats
		wantErr      bool
		wantAttempts int32
	}{
		{name: "429 twice then 200", statuses: []int{429, 429, 200}, wantAttempts: 3},
		{name: "500 then 200", statuses: []int{500, 200}, wantAttempts: 2},
		{name: "503 exhausts attempts", statuses: []int{503}, wantErr: true, wantAttempts: fetchMaxAttempts},
		{name: "404 is fatal, never retried", statuses: []int{404}, wantErr: true, wantAttempts: 1},
		{name: "403 is fatal, never retried", statuses: []int{403}, wantErr: true, wantAttempts: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(atomic.AddInt32(&hits, 1)) - 1
				code := tc.statuses[min(n, len(tc.statuses)-1)]
				w.WriteHeader(code)
				if code == http.StatusOK {
					fmt.Fprint(w, "payload")
				}
			}))
			defer srv.Close()

			g := newTestGithubClient()
			body, err := g.getBytes(context.Background(), srv.URL, "", false)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got body %q", body)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if string(body) != "payload" {
					t.Fatalf("body = %q, want %q", body, "payload")
				}
			}
			if got := atomic.LoadInt32(&hits); got != tc.wantAttempts {
				t.Fatalf("server hits = %d, want %d", got, tc.wantAttempts)
			}
		})
	}
}

// TestNextWait pins the wait-selection rule: exponential backoff, capped, but a
// server-supplied Retry-After always wins when it is longer. GitHub's raw 429s
// send no Retry-After at all, so the backoff must stand on its own.
func TestNextWait(t *testing.T) {
	tests := []struct {
		name           string
		backoff, retry time.Duration
		want           time.Duration
	}{
		{name: "no retry-after uses backoff", backoff: 500 * time.Millisecond, want: 500 * time.Millisecond},
		{name: "backoff is capped", backoff: time.Minute, want: fetchBackoffCap},
		{name: "longer retry-after wins over backoff", backoff: 500 * time.Millisecond, retry: 3 * time.Second, want: 3 * time.Second},
		{name: "longer retry-after wins over the cap too", backoff: time.Minute, retry: 30 * time.Second, want: 30 * time.Second},
		{name: "shorter retry-after does not shrink backoff", backoff: 2 * time.Second, retry: time.Millisecond, want: 2 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextWait(tc.backoff, tc.retry); got != tc.want {
				t.Fatalf("nextWait(%v, %v) = %v, want %v", tc.backoff, tc.retry, got, tc.want)
			}
		})
	}
}

// TestGetBytesObeysRetryAfter proves the header is actually honored on the wire,
// not merely parsed: a 1s Retry-After must delay the retry past the 1ms backoff.
func TestGetBytesObeysRetryAfter(t *testing.T) {
	shrinkBackoff(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	g := newTestGithubClient()
	start := time.Now()
	body, err := g.getBytes(context.Background(), srv.URL, "", false)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if string(body) != "payload" {
		t.Fatalf("body = %q", body)
	}
	if elapsed < time.Second {
		t.Fatalf("retried after %v; Retry-After: 1 was ignored (backoff is 1ms in this test)", elapsed)
	}
}

func TestGetBytesRespectsContextCancellation(t *testing.T) {
	shrinkBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	g := newTestGithubClient()
	if _, err := g.getBytes(ctx, srv.URL, "", false); err == nil {
		t.Fatal("expected error from a cancelled context")
	}
}

// TestRawContentsURL pins the Contents-API URL shape: path slashes stay literal,
// the ref is query-escaped.
func TestRawContentsURL(t *testing.T) {
	var gotPath, gotQuery, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAccept = r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept")
		fmt.Fprint(w, "yaml")
	}))
	defer srv.Close()

	g := newTestGithubClient()
	g.apiBase = srv.URL
	// auth=false is not an option for rawContents, so give it a token to reuse.
	g.token, g.tokenExp = "ghs_test", time.Now().Add(time.Hour)

	body, err := g.rawContents(context.Background(), "immich-app/immich", "docker/docker-compose.yml", "v3.0.1")
	if err != nil {
		t.Fatalf("rawContents: %v", err)
	}
	if string(body) != "yaml" {
		t.Fatalf("body = %q", body)
	}
	if want := "/repos/immich-app/immich/contents/docker/docker-compose.yml"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if want := "ref=v3.0.1"; gotQuery != want {
		t.Fatalf("query = %q, want %q", gotQuery, want)
	}
	if want := "application/vnd.github.raw"; gotAccept != want {
		t.Fatalf("Accept = %q, want %q", gotAccept, want)
	}
}

// TestReleaseQueryFor guards the latent bug: the resolver used to drop NameFilter
// and TagExclude, so it could resolve a tag detection had deliberately excluded.
func TestReleaseQueryFor(t *testing.T) {
	t.Run("all filter fields flow through", func(t *testing.T) {
		o := strategyOverride{
			GithubRepo:  "traefik/traefik",
			ReleaseType: "stable",
			NameFilter:  "STS",
			TagExclude:  "-ea",
		}
		q := releaseQueryFor("traefik/traefik", o)
		if q.Repo != "traefik/traefik" || q.ReleaseType != "stable" || q.NameFilter != "STS" || q.TagExclude != "-ea" {
			t.Fatalf("releaseQueryFor dropped a field: %+v", q)
		}
	})

	t.Run("empty release type defaults to latest", func(t *testing.T) {
		if q := releaseQueryFor("immich-app/immich", strategyOverride{}); q.ReleaseType != "latest" {
			t.Fatalf("ReleaseType = %q, want latest", q.ReleaseType)
		}
	})

	t.Run("repo argument wins over the override's own repo", func(t *testing.T) {
		o := strategyOverride{GithubRepo: "from/override"}
		if q := releaseQueryFor("auto/detected", o); q.Repo != "auto/detected" {
			t.Fatalf("Repo = %q, want auto/detected", q.Repo)
		}
	})
}
