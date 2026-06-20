//go:build smoke

// Phase 3.5 smoke test — exercises the stack-update engine end-to-end against a
// LIVE docker socket with a throwaway project (real stacks untouched). Uses the
// override resolver (deploy an exact tag) so it needs no GitHub/registry version
// resolution and is fully deterministic: it deploys alpine:3.19, then `update`s
// to alpine:3.20 and asserts the container swapped, came up healthy (alpine has
// no healthcheck → "none" passes), and the compose file was rewritten on disk.
//
//	cd docker-agent && DOCKER_HOST=unix:///var/run/docker.sock \
//	  go test -tags smoke -run TestUpdateSmoke -v
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateSmoke(t *testing.T) {
	root := t.TempDir()
	const name = "dockeragentupdsmoke"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, "docker-compose.yml")
	compose := "" +
		"services:\n" +
		"  app:\n" +
		"    image: alpine:3.19\n" +
		"    command: [\"sleep\", \"3600\"]\n"
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		NodeName:            "nuc",
		DockerHost:          os.Getenv("DOCKER_HOST"),
		ComposeRoot:         root,
		ComposeRegistryPath: filepath.Join(root, "projects.json"),
	}
	dc, err := newDockerClient(cfg.DockerHost)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer dc.close()
	cb, err := newComposeBackend(cfg)
	if err != nil {
		t.Fatalf("compose backend: %v", err)
	}
	reg := newComposeRegistry(cfg.ComposeRegistryPath)
	if err := reg.register(ProjectEntry{Name: name, WorkingDir: dir, ComposeFiles: []string{"docker-compose.yml"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	eng := newEngine(cfg, dc, cb, reg)
	noop := func() {}

	logJob := func(j *Job, label string) {
		t.Logf("--- %s log (state=%s) ---", label, j.snapshot().State)
		for _, l := range j.logLines() {
			t.Logf("  %s", l)
		}
	}

	// Deploy the initial version.
	upJob := &Job{jobPublic: jobPublic{ID: "smoke-up", Project: name, Operation: opComposeUp, State: JobPending}, subscribers: map[chan string]struct{}{}}
	upCtx, upCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	eng.runComposeOp(upCtx, upJob, opComposeUp, noop)
	upCancel()
	logJob(upJob, "up")
	if upJob.snapshot().State != JobCompleted {
		t.Fatalf("up not completed: %s", upJob.snapshot().ErrorMsg)
	}

	beforeID := singleContainerID(t, dc, name)

	// Update to a new exact tag via the override resolver.
	updJob := &Job{
		jobPublic:       jobPublic{ID: "smoke-update", Project: name, Operation: opComposeUpdate, State: JobPending},
		overrideImage:   "alpine:3.20",
		overrideService: "app",
		subscribers:     map[chan string]struct{}{},
	}
	updCtx, updCancel := context.WithTimeout(context.Background(), 4*time.Minute)
	eng.runUpdate(updCtx, updJob, noop)
	updCancel()
	logJob(updJob, "update")
	if updJob.snapshot().State != JobCompleted {
		t.Fatalf("update not completed: %s", updJob.snapshot().ErrorMsg)
	}

	afterID := singleContainerID(t, dc, name)
	if afterID == "" || afterID == beforeID {
		t.Fatalf("container did not swap: before=%s after=%s", beforeID, afterID)
	}

	// The compose file should be rewritten to the new tag (durable persistence).
	data, _ := os.ReadFile(composePath)
	if !strings.Contains(string(data), "alpine:3.20") {
		t.Fatalf("compose not rewritten to alpine:3.20:\n%s", data)
	}

	// Cleanup.
	downJob := &Job{jobPublic: jobPublic{ID: "smoke-down", Project: name, Operation: opComposeDown, State: JobPending}, subscribers: map[chan string]struct{}{}}
	dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
	eng.runComposeOp(dctx, downJob, opComposeDown, noop)
	dcancel()
	logJob(downJob, "down")
}

func singleContainerID(t *testing.T, dc *dockerClient, project string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	containers, _, err := dc.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, c := range containers {
		if c.ComposeProject == project {
			return c.ID
		}
	}
	return ""
}
