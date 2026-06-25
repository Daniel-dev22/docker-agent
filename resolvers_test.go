package main

import "testing"

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
