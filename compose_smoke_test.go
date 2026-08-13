//go:build smoke

// Smoke test — exercises the in-process compose binding against a
// LIVE docker socket with a throwaway project (real stacks untouched). Gated
// behind the `smoke` build tag so normal `go test ./...` never runs it.
//
//	cd docker-agent && DOCKER_HOST=unix:///var/run/docker.sock \
//	  go test -tags smoke -run TestComposeSmoke -v
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newSmokeJob(project, op string) *Job {
	return &Job{
		jobPublic:   jobPublic{ID: "smoke-" + op, Project: project, Operation: op, State: JobPending},
		subscribers: map[chan string]struct{}{},
	}
}

func TestComposeSmoke(t *testing.T) {
	root := t.TempDir()
	const name = "dockeragentsmoke"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "" +
		"services:\n" +
		"  app:\n" +
		"    image: alpine:latest\n" +
		"    command: [\"sleep\", \"3600\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
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
	reg := newComposeRegistry(cfg.ComposeRegistryPath, cfg.ComposeRoot)
	if err := reg.register(ProjectEntry{Name: name, WorkingDir: dir, ComposeFiles: []string{"docker-compose.yml"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	eng := newEngine(cfg, dc, cb, reg)
	noop := func() {}

	run := func(op string) *Job {
		j := newSmokeJob(name, op)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		eng.runComposeOp(ctx, j, op, noop)
		t.Logf("--- %s log ---", op)
		for _, l := range j.logLines() {
			t.Logf("  %s", l)
		}
		return j
	}

	// up
	if j := run(opComposeUp); j.snapshot().State != JobCompleted {
		t.Fatalf("up not completed: state=%s err=%s", j.snapshot().State, j.snapshot().ErrorMsg)
	}
	if !projectHasRunning(t, dc, name) {
		t.Fatalf("after up: project %q has no running container", name)
	}

	// pull (idempotent — image already present)
	if j := run(opComposePull); j.snapshot().State != JobCompleted {
		t.Fatalf("pull not completed: %s", j.snapshot().ErrorMsg)
	}

	// restart
	if j := run(opComposeRestart); j.snapshot().State != JobCompleted {
		t.Fatalf("restart not completed: %s", j.snapshot().ErrorMsg)
	}

	// down (cleanup)
	if j := run(opComposeDown); j.snapshot().State != JobCompleted {
		t.Fatalf("down not completed: %s", j.snapshot().ErrorMsg)
	}
	if projectHasRunning(t, dc, name) {
		t.Fatalf("after down: project %q still has a running container", name)
	}
}

func projectHasRunning(t *testing.T, dc *dockerClient, project string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, projects, err := dc.snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, p := range projects {
		if p.Name == project && p.RunningCount > 0 {
			return true
		}
	}
	return false
}
