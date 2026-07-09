package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// immichComposeFixture mirrors the shape of the real upstream file: two driver
// services templated on ${IMMICH_VERSION}, two coupled services pinned by digest.
const immichComposeFixture = `
name: immich
services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION:-release}
  immich-machine-learning:
    image: ghcr.io/immich-app/immich-machine-learning:${IMMICH_VERSION:-release}
  redis:
    image: docker.io/valkey/valkey:9@sha256:deadbeef
  database:
    image: ghcr.io/immich-app/postgres:14@sha256:cafebabe
volumes:
  model-cache:
`

// TestComposeImagesCachesByURL is the efficiency half of the 429 fix: a resolved
// URL embeds an immutable release tag, so repeated resolves (the 15-minute
// imagecheck pass, the update's re-resolve, the post-update ForceRecheck) must
// collapse onto ONE network fetch.
func TestComposeImagesCachesByURL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, immichComposeFixture)
	}))
	defer srv.Close()

	g := newTestGithubClient()
	vars := map[string]string{"IMMICH_VERSION": "v3.0.1"}
	url := srv.URL + "/immich-app/immich/v3.0.1/docker/docker-compose.yml"

	for i := range 3 {
		got, err := g.composeImages(context.Background(), url, vars)
		if err != nil {
			t.Fatalf("composeImages call %d: %v", i+1, err)
		}
		want := map[string]string{
			"immich-server":           "ghcr.io/immich-app/immich-server:v3.0.1",
			"immich-machine-learning": "ghcr.io/immich-app/immich-machine-learning:v3.0.1",
			"redis":                   "docker.io/valkey/valkey:9@sha256:deadbeef",
			"database":                "ghcr.io/immich-app/postgres:14@sha256:cafebabe",
		}
		if len(got) != len(want) {
			t.Fatalf("call %d: got %d services, want %d: %v", i+1, len(got), len(want), got)
		}
		for svc, img := range want {
			if got[svc] != img {
				t.Fatalf("call %d: %s = %q, want %q", i+1, svc, got[svc], img)
			}
		}
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server was hit %d times, want 1 (the tag's compose is immutable)", got)
	}
}

// A new release must mint a new key rather than serve the previous tag's images.
func TestComposeImagesNewTagRefetches(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, immichComposeFixture)
	}))
	defer srv.Close()

	g := newTestGithubClient()
	base := srv.URL + "/immich-app/immich/"

	old, err := g.composeImages(context.Background(), base+"v3.0.1/docker-compose.yml", map[string]string{"IMMICH_VERSION": "v3.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := g.composeImages(context.Background(), base+"v3.1.0/docker-compose.yml", map[string]string{"IMMICH_VERSION": "v3.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server was hit %d times, want 2 (distinct tags)", got)
	}
	if old["immich-server"] == next["immich-server"] {
		t.Fatalf("both tags resolved to %q — the cache leaked across tags", old["immich-server"])
	}
	if want := "ghcr.io/immich-app/immich-server:v3.1.0"; next["immich-server"] != want {
		t.Fatalf("new tag = %q, want %q", next["immich-server"], want)
	}
}

// The cache stores uninterpolated templates, so a cached entry must never leak an
// earlier caller's vars — and callers must never share a mutable map.
func TestComposeImagesCacheIsVarsIndependent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, immichComposeFixture)
	}))
	defer srv.Close()

	g := newTestGithubClient()
	url := srv.URL + "/o/n/ref/compose.yml"

	first, err := g.composeImages(context.Background(), url, map[string]string{"IMMICH_VERSION": "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	first["immich-server"] = "MUTATED" // a caller scribbling on its result

	second, err := g.composeImages(context.Background(), url, map[string]string{"IMMICH_VERSION": "v2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "ghcr.io/immich-app/immich-server:v2.0.0"; second["immich-server"] != want {
		t.Fatalf("second = %q, want %q (cache must hold templates, not interpolated images)", second["immich-server"], want)
	}
}

// A transient 429 on the compose body must be ridden out, not surfaced — this is
// the exact failure that killed the immich update job.
func TestComposeImagesRetriesThrough429(t *testing.T) {
	shrinkBackoff(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, immichComposeFixture)
	}))
	defer srv.Close()

	g := newTestGithubClient()
	got, err := g.composeImages(context.Background(), srv.URL+"/o/n/v1/compose.yml", map[string]string{"IMMICH_VERSION": "v1"})
	if err != nil {
		t.Fatalf("a bursty 429 must not fail the resolve: %v", err)
	}
	if got["immich-server"] != "ghcr.io/immich-app/immich-server:v1" {
		t.Fatalf("unexpected images after retry: %v", got)
	}
	if hits != 2 {
		t.Fatalf("server hits = %d, want 2", hits)
	}
}

// A failed fetch must not poison the cache with an empty entry.
func TestComposeImagesDoesNotCacheFailures(t *testing.T) {
	shrinkBackoff(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	g := newTestGithubClient()
	url := srv.URL + "/o/n/v1/compose.yml"
	if _, err := g.composeImages(context.Background(), url, nil); err == nil {
		t.Fatal("expected a 404 to fail")
	}
	if _, ok := g.cachedCompose(url); ok {
		t.Fatal("a failed fetch was cached")
	}
	if _, err := g.composeImages(context.Background(), url, nil); err == nil {
		t.Fatal("expected the second attempt to fail too")
	}
	if hits != 2 { // one per call; 404 is fatal so neither call retries
		t.Fatalf("server hits = %d, want 2", hits)
	}
}

func TestPlanServiceTarget(t *testing.T) {
	const img = "ghcr.io/blakeblackshear/frigate:8203e39-amd64"
	cases := []struct {
		name    string
		chk     imageCheck
		want    string
		wantErr bool
	}{
		{"outdated→rewrite", imageCheck{ImageStatus: statusOutdated, LatestImageVersion: "ghcr.io/blakeblackshear/frigate:ec3fb00-amd64"}, "ghcr.io/blakeblackshear/frigate:ec3fb00-amd64", false},
		{"updated→noop", imageCheck{ImageStatus: statusUpdated, LatestImageVersion: img}, "", false},
		{"outdated-but-equals-current→noop", imageCheck{ImageStatus: statusOutdated, LatestImageVersion: img}, "", false},
		// the bug: outdated but the resolver produced no target → LOUD failure, not a silent no-op.
		{"outdated-no-target→error", imageCheck{ImageStatus: statusOutdated, LatestImageVersion: ""}, "", true},
		// a version check that failed (e.g. fail-closed github) must NOT silently redeploy the pin.
		{"unknown→error", imageCheck{ImageStatus: statusUnknown, Error: "github-branch: list dev commits: 403"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := planServiceTarget("frigate", img, c.chk)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("target = %q, want %q", got, c.want)
			}
		})
	}
}

func TestInterpolateComposeVars(t *testing.T) {
	vars := map[string]string{"IMMICH_VERSION": "v2.7.5"}
	cases := []struct{ in, want string }{
		// immich's templated server image → concrete release tag
		{"ghcr.io/immich-app/immich-server:${IMMICH_VERSION:-release}", "ghcr.io/immich-app/immich-server:v2.7.5"},
		{"ghcr.io/immich-app/immich-server:${IMMICH_VERSION}", "ghcr.io/immich-app/immich-server:v2.7.5"},
		// unknown var falls back to the :-default
		{"docker.io/redis:${REDIS_TAG:-7-alpine}", "docker.io/redis:7-alpine"},
		// unknown var, bare form → empty
		{"repo:${MISSING}", "repo:"},
		// concrete @sha256 image untouched
		{"docker.io/valkey/valkey:9@sha256:abcd", "docker.io/valkey/valkey:9@sha256:abcd"},
		// dash (no colon) default form
		{"repo:${VER-1.0}", "repo:1.0"},
	}
	for _, c := range cases {
		if got := interpolateComposeVars(c.in, vars); got != c.want {
			t.Errorf("interpolateComposeVars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
