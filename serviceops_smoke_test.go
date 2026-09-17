//go:build smoke

// Smoke test — service-narrowed compose ops against a LIVE docker socket with a
// throwaway project (real stacks untouched):
//
//	cd docker-agent && DOCKER_HOST=unix:///var/run/docker.sock \
//	  go test -tags smoke -run TestServiceOpsSmoke -v
package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServiceOpsSmoke(t *testing.T) {
	root := t.TempDir()
	const name = "dockeragentsvcsmoke"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n" +
		"  a:\n    image: alpine:3.20\n    command: [\"sleep\", \"3600\"]\n" +
		"  b:\n    image: alpine:3.20\n    command: [\"sleep\", \"3600\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
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
	op := func(operation string, services ...string) {
		t.Helper()
		j := &Job{jobPublic: jobPublic{ID: operation, Project: name, Operation: operation, State: JobPending},
			services: services, subscribers: map[chan string]struct{}{}}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		eng.run(ctx, j, func(JobEvent) {}, func() {})
		if st := j.snapshot().State; st != JobCompleted {
			for _, l := range j.logLines() {
				t.Logf("  %s", l)
			}
			t.Fatalf("%s %v: %s %s", operation, services, st, j.snapshot().ErrorMsg)
		}
	}
	type svc struct{ id, state string }
	state := func() map[string]svc {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		containers, _, err := dc.snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]svc{}
		for _, c := range containers {
			if c.ComposeProject == name {
				out[c.ComposeService] = svc{c.ID, c.State}
			}
		}
		return out
	}
	network := func() bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		nets, err := dc.listNetworks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nets {
			if n.Name == name+"_default" {
				return true
			}
		}
		return false
	}
	defer func() {
		j := &Job{jobPublic: jobPublic{ID: "cleanup", Project: name, Operation: opComposeDown, State: JobPending}, subscribers: map[chan string]struct{}{}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		eng.run(ctx, j, func(JobEvent) {}, func() {})
	}()

	op(opComposeUp)
	s := state()
	if s["a"].state != "running" || s["b"].state != "running" {
		t.Fatalf("whole up: %v", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	zero := 0
	if err := dc.stopContainer(ctx, s["b"].id, &zero); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()

	op(opComposeRecreate, "a")
	after := state()
	if after["a"].id == s["a"].id || after["a"].state != "running" {
		t.Errorf("recreate [a] did not recreate a: %v", after)
	}
	if after["b"].id != s["b"].id || after["b"].state != "exited" {
		t.Errorf("recreate [a] touched the operator-stopped b: %v", after)
	}

	op(opComposeDown, "a")
	after = state()
	if _, ok := after["a"]; ok {
		t.Errorf("down [a] left a's container: %v", after)
	}
	if after["b"].id != s["b"].id {
		t.Errorf("down [a] removed b: %v", after)
	}
	if !network() {
		t.Error("down [a] removed the project's network")
	}

	op(opComposeUp, "a")
	after = state()
	if after["a"].state != "running" || after["b"].state != "exited" {
		t.Errorf("up [a] with b stopped: %v — only a may start", after)
	}

	op(opComposeRestart, "a")
	op(opComposePull, "a")
	after = state()
	if after["b"].state != "exited" {
		t.Errorf("restart/pull [a] started b: %v", after)
	}

	// The same project seen from an agent whose compose root does NOT contain it:
	// its files are unreadable to that agent, so narrowed restart and down must work
	// from the containers' labels alone.
	outsideRoot := t.TempDir()
	outCfg := Config{NodeName: "nuc", DockerHost: cfg.DockerHost, ComposeRoot: outsideRoot, ComposeRegistryPath: filepath.Join(outsideRoot, "projects.json")}
	outCB, err := newComposeBackend(outCfg)
	if err != nil {
		t.Fatal(err)
	}
	outReg := newComposeRegistry(outCfg.ComposeRegistryPath, outsideRoot)
	if err := outReg.register(ProjectEntry{Name: name, WorkingDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := outCB.loadProject(context.Background(), ProjectEntry{Name: name, WorkingDir: dir}); err == nil {
		t.Fatal("fixture: the outside agent can read the project's files, so this proves nothing")
	}
	outEng := newEngine(outCfg, dc, outCB, outReg)
	outOp := func(operation string, services ...string) *Job {
		t.Helper()
		j := &Job{jobPublic: jobPublic{ID: "outside-" + operation, Project: name, Operation: operation, State: JobPending, Services: services},
			services: services, subscribers: map[chan string]struct{}{}}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		outEng.run(ctx, j, func(JobEvent) {}, func() {})
		if st := j.snapshot().State; st != JobCompleted {
			for _, l := range j.logLines() {
				t.Logf("  %s", l)
			}
			t.Fatalf("outside %s %v: %s %s", operation, services, st, j.snapshot().ErrorMsg)
		}
		return j
	}
	before := state()
	rj := outOp(opComposeRestart, "a")
	if !slices.ContainsFunc(rj.logLines(), func(l string) bool { return strings.HasPrefix(l, "compose restart "+name+" [a]") }) {
		t.Errorf("the job log does not name the narrowed services: %v", rj.logLines())
	}
	after = state()
	if after["a"].state != "running" || after["a"].id != before["a"].id || after["b"].state != "exited" {
		t.Errorf("outside restart [a]: %v (before %v)", after, before)
	}
	outOp(opComposeDown, "a")
	after = state()
	if _, ok := after["a"]; ok {
		t.Errorf("outside down [a] left a: %v", after)
	}
	if after["b"].id != before["b"].id {
		t.Errorf("outside down [a] removed b: %v", after)
	}
}
