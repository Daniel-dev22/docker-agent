//go:build smoke

// Smoke test — exercises the stack-update engine end-to-end against a
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
	reg := newComposeRegistry(cfg.ComposeRegistryPath, cfg.ComposeRoot)
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
	// The engine deploys the project as it reloads from disk after the write: the
	// swapped container must run the new image, not the one loaded before it.
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	containers, _, err := dc.snapshot(sctx)
	scancel()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range containers {
		if c.ComposeProject == name && c.Image != "alpine:3.20" {
			t.Fatalf("the swapped container runs %q, want alpine:3.20", c.Image)
		}
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

// TestUpdateSmokeRollbackRestoresBytes: an update whose new image cannot start is
// rolled back, and the compose file it wrote is back to its exact original bytes —
// irregular formatting and comments included — with the stack on its old image.
func TestUpdateSmokeRollbackRestoresBytes(t *testing.T) {
	root := t.TempDir()
	const name = "dockeragentrbsmoke"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, "docker-compose.yml")
	// hello-world has no `sleep`: the new container cannot start.
	compose := "services:\n\n  app:   # smoke\n     image: alpine:3.19   # pinned\n     command: [\"sleep\", \"3600\"]\n"
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{NodeName: "nuc", DockerHost: os.Getenv("DOCKER_HOST"), ComposeRoot: root, ComposeRegistryPath: filepath.Join(root, "projects.json")}
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
	run := func(j *Job, d time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		eng.run(ctx, j, func(JobEvent) {}, noop)
		t.Logf("--- %s (state=%s) ---", j.snapshot().ID, j.snapshot().State)
		for _, l := range j.logLines() {
			t.Logf("  %s", l)
		}
	}
	newJob := func(id, op, image string) *Job {
		return &Job{jobPublic: jobPublic{ID: id, Project: name, Operation: op, State: JobPending},
			overrideImage: image, overrideService: "app", subscribers: map[chan string]struct{}{}}
	}
	defer run(newJob("rb-down", opComposeDown, ""), time.Minute)

	up := newJob("rb-up", opComposeUp, "")
	run(up, 2*time.Minute)
	if up.snapshot().State != JobCompleted {
		t.Fatalf("up: %s", up.snapshot().ErrorMsg)
	}
	upd := newJob("rb-update", opComposeUpdate, "hello-world:latest")
	run(upd, 5*time.Minute)
	if upd.snapshot().State != JobFailed {
		t.Fatalf("an update to an image that cannot start must fail, got %s", upd.snapshot().State)
	}
	if data, _ := os.ReadFile(composePath); string(data) != compose {
		t.Fatalf("the compose file is not back to its original bytes:\n%q\nwant\n%q", data, compose)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	containers, _, err := dc.snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range containers {
		if c.ComposeProject == name && c.State != "running" {
			t.Errorf("after the rollback %s is %s", c.Name, c.State)
		}
	}
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
