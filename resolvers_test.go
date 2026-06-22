package main

import "testing"

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
