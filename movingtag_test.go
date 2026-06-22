package main

import "testing"

// A coupled stack's driver on a moving tag (immich-server ":release") auto-tracks
// latest, so the upstream-compose plan must NOT flag it to a concrete pin — only
// concretely-pinned coupled services drive the stack's update status.
func TestTracksMovingTag(t *testing.T) {
	moving := []string{
		"ghcr.io/immich-app/immich-server:release",
		"ghcr.io/immich-app/immich-machine-learning:release",
		"nginx:latest",
		"x:edge",
		"y:stable",
	}
	concrete := []string{
		"ghcr.io/immich-app/immich-server:v2.7.5",
		"docker.io/valkey/valkey:9@sha256:3b55fbaa",
		"ghcr.io/immich-app/postgres:14-vectorchord0.4.3@sha256:bcf63357",
		"redis:7.2.4",
		"postgres:14",
	}
	for _, img := range moving {
		if !tracksMovingTag(img) {
			t.Errorf("tracksMovingTag(%q) = false, want true (moving tag → skip in coupled plan)", img)
		}
	}
	for _, img := range concrete {
		if tracksMovingTag(img) {
			t.Errorf("tracksMovingTag(%q) = true, want false (concrete pin → driven by release compose)", img)
		}
	}
}
