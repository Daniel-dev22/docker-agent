package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ownership has to survive the index that carries it. Phase 2 recorded the owner
// only in projects.json; these pin the three ways that record was lost and the
// files Ansible renders became editable again — a lost registry, a deregister,
// and an op resolved against a name the index no longer holds.

// liveOf is the label-derived project docker would report for a running stack —
// what boot adoption sees, and the only thing it sees. It carries no owner.
func liveOf(name, dir string) ComposeProject {
	return ComposeProject{Name: name, WorkingDir: dir, ConfigFiles: filepath.Join(dir, "docker-compose.yml")}
}

// loseRegistry deletes projects.json and rebuilds the registry from the running
// containers, exactly as the agent does when it restarts: load (nothing) then
// adopt from labels. It returns the new registry and installs it on the app, so
// an HTTP call after it is a call to an agent that has forgotten.
func loseRegistry(t *testing.T, e *capEnv, live ...ComposeProject) *composeRegistry {
	t.Helper()
	must(t, os.Remove(e.a.cfg.ComposeRegistryPath))
	fresh := newComposeRegistry(e.a.cfg.ComposeRegistryPath, e.root)
	must(t, fresh.load())
	fresh.enrichFromLive(live)
	e.a.projects = fresh
	return fresh
}

func markOf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ownerMarkName))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read owner mark: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// TestOwnershipSurvivesLosingTheRegistry is the Phase 3 reproduction as a test.
// Measured against the agent before this change (throwaway instance, 2026-09-20):
// `rm projects.json` + restart brought an owner:"ansible" stack back with
// owner:null and managed:true, and the unowned entry was persisted.
func TestOwnershipSurvivesLosingTheRegistry(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	if got := markOf(t, dir); got != "ansible" {
		t.Fatalf("register did not record the owner in its directory: %q", got)
	}

	fresh := loseRegistry(t, e, liveOf("gdrive-agent", dir))

	entry, ok := fresh.get("gdrive-agent")
	if !ok {
		t.Fatal("the stack was not re-adopted from its containers")
	}
	if entry.Owner != "ansible" {
		t.Errorf("owner = %q after losing the registry, want \"ansible\"", entry.Owner)
	}
	capa := projectCapabilities(entry, e.root, nil)
	if capa.Editable {
		t.Error("a re-adopted Ansible-rendered stack must not be editable")
	}
	if !capa.Readable || !capa.operable() {
		t.Errorf("it must stay readable and operable: %+v", capa)
	}
	// The re-adopted entry is persisted; the owner has to be persisted with it, or
	// the next restart is the one that loses it.
	var persisted []ProjectEntry
	raw, err := os.ReadFile(e.a.cfg.ComposeRegistryPath)
	must(t, err)
	must(t, json.Unmarshal(raw, &persisted))
	for _, p := range persisted {
		if p.Name == "gdrive-agent" && p.Owner != "ansible" {
			t.Errorf("re-persisted entry lost the owner: %+v", p)
		}
	}
}

// TestEditorSaveIsRefusedAfterTheRegistryIsLost is the harm, not the flag. The
// editor's exact call rewrote the compose file on disk in the reproduction; with
// the owner recorded in the directory the same call is refused.
func TestEditorSaveIsRefusedAfterTheRegistryIsLost(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	before, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)

	loseRegistry(t, e, liveOf("gdrive-agent", dir))

	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "gdrive-agent", "replace": true,
		"files": map[string]string{"docker-compose.yml": "services: {}\n"},
	})
	if status != http.StatusConflict || body["code"] != "project_owned" {
		t.Fatalf("editor save after a registry loss: %d %v, want 409 project_owned", status, body)
	}
	after, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)
	if string(after) != string(before) {
		t.Error("a refused save rewrote the owner's compose file")
	}
}

// TestDeregisterRefusesAnOwnedStack closes the laundering path that needed no
// loss at all: DELETE dropped the entry carrying the owner while the containers
// kept running, and the next snapshot rebuilt the row as editable.
func TestDeregisterRefusesAnOwnedStack(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")

	status, body := e.do(t, http.MethodDelete, "/v1/projects/gdrive-agent", nil)
	if status != http.StatusConflict || body["code"] != "project_owned" {
		t.Fatalf("deregister of an owned stack: %d %v, want 409 project_owned", status, body)
	}
	if _, ok := e.a.projects.get("gdrive-agent"); !ok {
		t.Fatal("a refused deregister dropped the entry anyway")
	}

	// The documented way out, and the only one: take the files back first.
	status, body = e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "gdrive-agent", "working_dir": dir, "replace": true, "owner": "",
	})
	if status != http.StatusOK {
		t.Fatalf("clearing the owner: %d %v", status, body)
	}
	if got := markOf(t, dir); got != "" {
		t.Errorf("clearing the owner left a mark on disk: %q", got)
	}
	status, body = e.do(t, http.MethodDelete, "/v1/projects/gdrive-agent", nil)
	if status != http.StatusOK {
		t.Fatalf("deregister after clearing the owner: %d %v", status, body)
	}
	if _, ok := e.a.projects.get("gdrive-agent"); ok {
		t.Error("the entry survived a successful deregister")
	}
}

// TestDeregisterOfAnUnknownProjectStaysIdempotent — callers that clean up after
// themselves depend on it, and the new lock/owner path must not change it.
func TestDeregisterOfAnUnknownProjectStaysIdempotent(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	status, body := e.do(t, http.MethodDelete, "/v1/projects/never-registered", nil)
	if status != http.StatusOK {
		t.Fatalf("deregister of an unknown project: %d %v, want 200", status, body)
	}
}

// TestResolveCarriesTheDirectoryOwner: an op for a name the index does not hold
// falls back to the live containers. That entry is what a write would be checked
// against, so it needs the owner too.
func TestResolveCarriesTheDirectoryOwner(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	empty := newComposeRegistry(filepath.Join(t.TempDir(), "projects.json"), e.root)

	entry, ok := empty.resolve("gdrive-agent", []ComposeProject{liveOf("gdrive-agent", dir)})
	if !ok {
		t.Fatal("resolve did not fall back to the live project")
	}
	if entry.Owner != "ansible" {
		t.Errorf("resolved owner = %q, want \"ansible\"", entry.Owner)
	}
	if projectCapabilities(entry, e.root, nil).Editable {
		t.Error("a live-resolved owned stack must not be editable")
	}
}

// TestSnapshotRowCarriesTheDirectoryOwner: between a deregister and the next
// adoption the fleet row is built from labels alone. That row is what the UI
// reads to decide whether to offer Edit.
func TestSnapshotRowCarriesTheDirectoryOwner(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	empty := newComposeRegistry(filepath.Join(t.TempDir(), "projects.json"), e.root)

	merged := empty.mergeKnown([]ComposeProject{liveOf("gdrive-agent", dir)}, nil)
	if len(merged) != 1 {
		t.Fatalf("merged = %d rows, want 1", len(merged))
	}
	if merged[0].Owner != "ansible" {
		t.Errorf("snapshot owner = %q, want \"ansible\"", merged[0].Owner)
	}
	if merged[0].Managed {
		t.Error("the snapshot offered Edit on an Ansible-rendered stack")
	}
}

// TestLoadRecordsAnOwnerTheDirectoryDoesNotKnow is the migration: every stack the
// fleet already converged gets its mark on the first restart after this ships, with
// no role run and no operator step.
func TestLoadRecordsAnOwnerTheDirectoryDoesNotKnow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gdrive-agent")
	must(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(root, "projects.json")
	must(t, os.WriteFile(path, []byte(`[{"name":"gdrive-agent","working_dir":"`+dir+`","owner":"ansible"}]`), 0o600))

	reg := newComposeRegistry(path, root)
	must(t, reg.load())

	if got := markOf(t, dir); got != "ansible" {
		t.Errorf("load did not record the index's owner in its directory: %q", got)
	}
	if got := reg.unmarkedOwners(); len(got) != 0 {
		t.Errorf("readiness still reports unrecorded owners after the migration: %v", got)
	}
}

// TestTheDirectoryOutranksTheIndex: the index is the copy that can be lost or
// rolled back, so a disagreement resolves to the directory.
func TestTheDirectoryOutranksTheIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gdrive-agent")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, writeOwnerMark(dir, root, "ansible"))
	path := filepath.Join(root, "projects.json")
	must(t, os.WriteFile(path, []byte(`[{"name":"gdrive-agent","working_dir":"`+dir+`"}]`), 0o600))

	reg := newComposeRegistry(path, root)
	must(t, reg.load())

	entry, ok := reg.get("gdrive-agent")
	if !ok || entry.Owner != "ansible" {
		t.Fatalf("the directory did not win: %+v", entry)
	}
	// And the correction is persisted, so the wire value does not depend on a read
	// that happened to run this boot.
	raw, err := os.ReadFile(path)
	must(t, err)
	var persisted []ProjectEntry
	must(t, json.Unmarshal(raw, &persisted))
	if len(persisted) != 1 || persisted[0].Owner != "ansible" {
		t.Errorf("the reconciled owner was not persisted: %s", raw)
	}
}

// TestAMarkThatCannotBeUnderstoodFailsSafe. A directory that says something about
// who renders it and cannot be read is not one the agent may rewrite. Every case
// resolves to a NON-EMPTY owner, because "" is the answer that unlocks the editor.
func TestAMarkThatCannotBeUnderstoodFailsSafe(t *testing.T) {
	cases := map[string]string{
		"empty":               "",
		"whitespace":          "   \n",
		"uppercase":           "Ansible",
		"spaces":              "the ansible role",
		"too-long":            strings.Repeat("a", maxOwnerLen+1),
		"past-the-read-bound": strings.Repeat("a", maxOwnerMarkBytes+1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "stack")
			must(t, os.MkdirAll(dir, 0o755))
			must(t, os.WriteFile(filepath.Join(dir, ownerMarkName), []byte(content), 0o600))

			owner, err := readOwnerMark(dir, root)
			if err == nil {
				t.Fatalf("a %s mark was accepted as %q", name, owner)
			}
			if owner != unknownOwner {
				t.Errorf("owner = %q, want %q", owner, unknownOwner)
			}
			if projectCapabilities(ProjectEntry{Name: "stack", WorkingDir: dir, Owner: owner}, root, nil).Editable {
				t.Error("a stack with an unreadable mark was editable")
			}
		})
	}

	t.Run("a-directory", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "stack")
		must(t, os.MkdirAll(filepath.Join(dir, ownerMarkName), 0o755))
		owner, err := readOwnerMark(dir, root)
		if err == nil || owner != unknownOwner {
			t.Errorf("a mark that is a directory: owner=%q err=%v", owner, err)
		}
	})
}

// TestAMarkOutsideTheComposeRootIsNotRead. Ownership governs what the agent
// rewrites, and it rewrites nothing outside the root — so a working dir taken from
// a container label cannot point the read at an arbitrary path on the host.
func TestAMarkOutsideTheComposeRootIsNotRead(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, ownerMarkName), []byte("ansible\n"), 0o600))

	owner, err := readOwnerMark(outside, root)
	if owner != "" || err != nil {
		t.Errorf("readOwnerMark outside the root = %q, %v; want \"\", nil", owner, err)
	}
	// And writing there is a no-op rather than an error, so an external stack can
	// still be registered with an owner for the wire.
	must(t, writeOwnerMark(outside, root, "ansible"))
}

// TestAMarkSymlinkedOutOfTheRootFailsSafe — the same confinement every other read
// in projectfiles.go applies, on the one file whose answer unlocks the editor.
func TestAMarkSymlinkedOutOfTheRootFailsSafe(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dir := filepath.Join(root, "stack")
	must(t, os.MkdirAll(dir, 0o755))
	target := filepath.Join(outside, "owner")
	must(t, os.WriteFile(target, []byte("ansible\n"), 0o600))
	must(t, os.Symlink(target, filepath.Join(dir, ownerMarkName)))

	owner, err := readOwnerMark(dir, root)
	if err == nil {
		t.Fatalf("a mark symlinked out of the root was accepted as %q", owner)
	}
	if owner != unknownOwner {
		t.Errorf("owner = %q, want %q", owner, unknownOwner)
	}
}

// TestTheOwnerMarkIsReservedInABundle: anything that can push files — the editor,
// a cross-host copy — must not be able to declare an owner by writing the file.
func TestTheOwnerMarkIsReservedInABundle(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	for _, path := range []string{ownerMarkName, "nested/" + ownerMarkName, "./" + ownerMarkName} {
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
			"name": "forged", "replace": true,
			"files": map[string]string{"docker-compose.yml": composeA, path: "ansible\n"},
		})
		if status != http.StatusBadRequest || body["code"] != "invalid_file_path" {
			t.Errorf("bundle path %q: %d %v, want 400 invalid_file_path", path, status, body)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "forged")); err == nil {
		t.Error("a refused bundle created the project directory anyway")
	}
}

// TestCopyRefusesIntoAnOwnedDirectory: a copy writes the destination's files, and
// the destination has no index entry by definition — so only the directory can say
// the files belong to someone.
func TestCopyRefusesIntoAnOwnedDirectory(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	e.writeCompose(t, "source", composeA)
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "source", "working_dir": filepath.Join(e.root, "source"), "replace": true,
	})
	if status != http.StatusOK {
		t.Fatalf("register source: %d %v", status, body)
	}
	// A directory that records an owner but has no entry: what a lost registry, or
	// a deregister that predates this change, leaves behind.
	dest := filepath.Join(e.root, "gdrive-agent")
	must(t, os.MkdirAll(dest, 0o755))
	must(t, writeOwnerMark(dest, e.root, "ansible"))

	status, body = e.do(t, http.MethodPost, "/v1/projects/source/copy", map[string]any{"new_name": "gdrive-agent"})
	if status != http.StatusConflict || body["code"] != "project_owned" {
		t.Fatalf("copy into an owned directory: %d %v, want 409 project_owned", status, body)
	}
	if _, err := os.Stat(filepath.Join(dest, "docker-compose.yml")); err == nil {
		t.Error("the refused copy wrote its files anyway")
	}
}

// TestAnOwnerRegisteredOnceIsNotRewrittenEveryTime: the mark write is on the
// register's path, so it must be skipped when nothing about the owner changed —
// an editor save on an unowned stack must not touch the filesystem for it.
func TestAnOwnerRegisteredOnceIsNotRewrittenEveryTime(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := e.writeCompose(t, "plain", composeA)
	status, _ := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "plain", "working_dir": dir, "replace": true,
	})
	if status != http.StatusOK {
		t.Fatalf("register: %d", status)
	}
	if _, err := os.Stat(filepath.Join(dir, ownerMarkName)); !os.IsNotExist(err) {
		t.Errorf("an unowned register created an owner mark: %v", err)
	}
}

// TestReadinessReportsAnOwnerItCouldNotRecord is the negative control for the
// readiness signal: a list that is always empty reports nothing and looks healthy.
// An owner the index holds and the directory does not is exactly the state that
// would not survive a registry loss, and the mark write failing is otherwise
// silent until the day it matters.
func TestReadinessReportsAnOwnerItCouldNotRecord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gdrive-agent")
	must(t, os.MkdirAll(dir, 0o755))
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)
	// Straight into the index, bypassing the register that would have written the
	// mark — which is what a failed mark write leaves behind.
	must(t, reg.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: dir, Owner: "ansible"}))
	must(t, os.Remove(filepath.Join(dir, ownerMarkName))) // as a failed mark write leaves it
	reg.refreshShared()

	got := reg.unmarkedOwners()
	if len(got) != 1 || got[0] != "gdrive-agent" {
		t.Fatalf("unmarkedOwners = %v, want [gdrive-agent]", got)
	}
	// And it clears once the directory records it, so the signal is a state and not
	// a latch.
	must(t, writeOwnerMark(dir, root, "ansible"))
	reg.refreshShared()
	if got := reg.unmarkedOwners(); len(got) != 0 {
		t.Errorf("unmarkedOwners = %v after the mark was written, want empty", got)
	}
}

// TestDeregisterWaitsForTheProjectLock. Deregister took no lock, so it could drop
// the entry an in-flight register was about to re-read the owner from — the exact
// window that register's under-lock re-read exists to close, reached from the
// other side. The observable is direct: without the lock this answers 200 while
// another change holds the project.
func TestDeregisterWaitsForTheProjectLock(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	requestLockWait = 50 * time.Millisecond
	defer func() { requestLockWait = 10 * time.Second }()
	dir := e.writeCompose(t, "plain", composeA)
	if status, _ := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "plain", "working_dir": dir, "replace": true,
	}); status != http.StatusOK {
		t.Fatal("fixture register failed")
	}
	entry, _ := e.a.projects.get("plain")

	release, err := e.a.reg.eng.locks.acquire(context.Background(), projectLockKey(entry), "job 42 (update)", nil)
	must(t, err)
	status, body := e.do(t, http.MethodDelete, "/v1/projects/plain", nil)
	release()

	if status != http.StatusConflict || body["code"] != "project_busy" || body["holder"] != "job 42 (update)" {
		t.Fatalf("deregister while the project is locked: %d %v, want 409 project_busy", status, body)
	}
	if _, ok := e.a.projects.get("plain"); !ok {
		t.Error("a busy deregister dropped the entry anyway")
	}
}

// TestEditorSaveIsRefusedForAStoppedStackAfterTheRegistryIsLost is why the write
// guard asks the DIRECTORY and not the index it just rebuilt.
//
// Adoption only sees RUNNING projects, so for a stopped owned stack a lost
// projects.json leaves nothing at all: no entry, no containers, no owner. Exactly
// the same for the window at boot before adoption has run. The index cannot
// answer "who do these files belong to" in either case — only the directory the
// files are in can. A canary sweep found this: reverting the guard to the index
// alone left TestEditorSaveIsRefusedAfterTheRegistryIsLost green, because
// adoption had put the owner back before the save arrived.
func TestEditorSaveIsRefusedForAStoppedStackAfterTheRegistryIsLost(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	before, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)

	loseRegistry(t, e) // no live projects: nothing to adopt, so the index stays empty

	if _, ok := e.a.projects.get("gdrive-agent"); ok {
		t.Fatal("fixture: the index was supposed to be empty")
	}
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "gdrive-agent", "replace": true,
		"files": map[string]string{"docker-compose.yml": "services: {}\n"},
	})
	if status != http.StatusConflict || body["code"] != "project_owned" {
		t.Fatalf("editor save onto a stopped owned stack: %d %v, want 409 project_owned", status, body)
	}
	after, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)
	if string(after) != string(before) {
		t.Error("a refused save rewrote the owner's compose file")
	}
}

// TestAnOwnedWriteIsRefusedWithoutWaitingForTheLock pins the EARLY refusal, which
// is otherwise invisible: the decision is the re-read under the lock, so removing
// the cheap check before it changes no verdict — only how long a doomed request
// holds a connection, and which answer it gets. `project_owned` is permanent and
// `project_busy` is retryable, so an owner's converge told the wrong one retries
// a write that can never succeed.
func TestAnOwnedWriteIsRefusedWithoutWaitingForTheLock(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	requestLockWait = 2 * time.Second // long enough that waiting it out would show
	defer func() { requestLockWait = 10 * time.Second }()
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	entry, _ := e.a.projects.get("gdrive-agent")

	release, err := e.a.reg.eng.locks.acquire(context.Background(), projectLockKey(entry), "job 42 (update)", nil)
	must(t, err)
	defer release()

	started := time.Now()
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "gdrive-agent", "replace": true,
		"files": map[string]string{"docker-compose.yml": "services: {}\n"},
	})
	elapsed := time.Since(started)

	if status != http.StatusConflict || body["code"] != "project_owned" {
		t.Fatalf("owned write while the project is locked: %d %v, want 409 project_owned", status, body)
	}
	if elapsed >= requestLockWait {
		t.Errorf("it waited %v for a lock it did not need", elapsed)
	}
	_ = dir
}
