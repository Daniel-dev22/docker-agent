//go:build smoke

// Phase 3 smoke test — exercises the registry-digest VersionSource + the
// existence gate against the REAL Docker Hub registry and a LIVE docker socket
// (read-only: inspects local images, no mutations). GitHub/controller paths are
// NOT exercised here (no creds in a local run); those are unit-tested for logic
// and verified in deploy. Gated behind the `smoke` build tag.
//
//	cd docker-agent && DOCKER_HOST=unix:///var/run/docker.sock \
//	  go test -tags smoke -run TestImageCheckSmoke -v
package main

import (
	"context"
	"testing"
	"time"
)

func TestImageCheckSmoke(t *testing.T) {
	dc, err := newDockerClient("")
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer dc.close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ensure a known moving-tag image is present locally.
	containers, _, err := dc.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Logf("live containers: %d", len(containers))

	// 1) Local inspect → RepoDigests for alpine:latest.
	info, err := dc.inspectImage(ctx, "alpine:latest")
	if err != nil {
		t.Fatalf("inspect alpine:latest (run `docker pull alpine` first): %v", err)
	}
	t.Logf("alpine RepoDigests=%v labels=%d", info.RepoDigests, len(info.Labels))

	reg := newRegistryClient("", nil)

	// 2) Registry-digest path: local vs remote for alpine:latest.
	ref := parseImageReference("alpine:latest", "latest")
	remote, err := reg.getRegistryDigest(ctx, ref)
	if err != nil {
		t.Fatalf("getRegistryDigest alpine:latest: %v", err)
	}
	t.Logf("alpine:latest remote digest=%s", remote)
	if remote == "" {
		t.Fatal("empty remote digest")
	}

	res := imageCheck{Image: "alpine:latest", ImageStatus: statusUnknown}
	(&imageChecker{registry: reg}).fillRegistryDigest(ctx, &res, info, ref)
	t.Logf("alpine:latest status=%s latest=%s err=%s", res.ImageStatus, res.LatestImageVersion, res.Error)
	if res.ImageStatus != statusUpdated && res.ImageStatus != statusOutdated {
		t.Errorf("expected updated|outdated, got %q (err=%s)", res.ImageStatus, res.Error)
	}

	// 3) Existence gate: a real tag exists, a bogus one does not.
	if !reg.imageExists(ctx, parseImageReference("library/alpine:3.20", "latest")) {
		t.Error("alpine:3.20 should exist on Docker Hub")
	}
	if reg.imageExists(ctx, parseImageReference("library/alpine:does-not-exist-9999", "latest")) {
		t.Error("bogus alpine tag should NOT exist")
	}

	// 4) Auto-detect: alpine:latest (moving) → registry-digest.
	ic := &imageChecker{overrides: map[string]strategyOverride{}}
	s := ic.resolveStrategy(ContainerStatus{Image: "alpine:latest"}, info, ref)
	if s.source != "registry-digest" {
		t.Errorf("alpine:latest auto-detect = %q, want registry-digest", s.source)
	}
	t.Logf("alpine:latest strategy=%s", s.label())
}
