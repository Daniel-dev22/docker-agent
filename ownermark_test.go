package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// TestDeregisterCannotMakeAnOwnedStackEditable is what the deregister path has
// to guarantee, and it is a property of the DIRECTORY rather than a refusal.
//
// Before the mark, DELETE was a one-call ownership launder: the entry carrying
// the owner disappeared, the containers kept running, and the very next fleet
// snapshot rebuilt the row as unowned and editable — measured 2026-09-20. An
// owner CHECK on the delete was written first and then removed: it refused the
// drop, which cost a shared working directory its only exit and broke the
// documented teardown of a stack whose files are gone, and it bought nothing,
// because the row is rebuilt from the directory either way.
func TestDeregisterCannotMakeAnOwnedStackEditable(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")

	status, body := e.do(t, http.MethodDelete, "/v1/projects/gdrive-agent", nil)
	if status != http.StatusOK {
		t.Fatalf("deregister: %d %v, want 200", status, body)
	}
	if _, ok := e.a.projects.get("gdrive-agent"); ok {
		t.Fatal("the entry survived a deregister")
	}

	// 1. the live row the UI reads
	merged := e.a.projects.mergeKnown([]ComposeProject{liveOf("gdrive-agent", dir)}, nil)
	if merged[0].Owner != "ansible" || merged[0].Managed {
		t.Errorf("the snapshot offered Edit after a deregister: %+v", merged[0])
	}
	// 2. the write itself
	before, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)
	status, body = e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "gdrive-agent", "replace": true,
		"files": map[string]string{"docker-compose.yml": "services: {}\n"},
	})
	if status != http.StatusConflict || body["code"] != "project_owned" {
		t.Fatalf("editor save after a deregister: %d %v, want 409 project_owned", status, body)
	}
	after, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)
	if string(after) != string(before) {
		t.Error("a refused save rewrote the owner's compose file")
	}
	// 3. and the way out still works, in one call
	status, body = e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "gdrive-agent", "working_dir": dir, "replace": true, "owner": "",
	})
	if status != http.StatusOK {
		t.Fatalf("clearing the owner: %d %v", status, body)
	}
	if got := markOf(t, dir); got != "" {
		t.Errorf("clearing the owner left a mark on disk: %q", got)
	}
}

// TestDeregisterOfATornDownStackStillWorks: a role that removes a stack's files
// and then deregisters it is a documented op (docker_agent_stack op=deregister).
// An owner check on the delete broke it — the entry still carried the owner, the
// directory was gone, and the index kept a dead entry forever.
func TestDeregisterOfATornDownStackStillWorks(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	must(t, os.RemoveAll(dir))

	status, body := e.do(t, http.MethodDelete, "/v1/projects/gdrive-agent", nil)
	if status != http.StatusOK {
		t.Fatalf("deregister of a torn-down owned stack: %d %v, want 200", status, body)
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
			t.Fatalf("a mark that is a directory: owner=%q err=%v", owner, err)
		}
		// The read would fail on its own (EISDIR), so the explicit regular-file
		// check earns its place by naming the CAUSE rather than reporting a
		// malformed owner. A refusal that blames the wrong thing sends whoever
		// reads it looking at the file's contents.
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("the failure does not name the cause: %v", err)
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

// ---------------------------------------------------------------------------
// Review findings. Each of these fails against the first version of this change.
// ---------------------------------------------------------------------------

// TestAnUnreadableMarkIsNeverPersisted is the defect three independent review
// lenses found, from three directions.
//
// readOwnerMark answers unknownOwner for a mark it cannot read, so a write guard
// refuses — that is right. Recording it was not: reconcile wrote the sentinel
// into the index, and the next boot's migration branch then wrote it into the
// DIRECTORY as though a tool had declared it, destroying the real owner and
// refusing the owner's own converge (`"ansible" != "unknown"`). Readiness read
// healthy throughout, because a mark was present.
//
// The trigger is not exotic: confinePath resolves the compose root, so a root
// that is briefly unresolvable — a mount not up yet when the agent starts —
// fails this way for EVERY entry at once.
func TestAnUnreadableMarkIsNeverPersisted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gdrive-agent")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ownerMarkName), []byte("not a valid owner!\n"), 0o600))
	path := filepath.Join(root, "projects.json")
	must(t, os.WriteFile(path, []byte(`[{"name":"gdrive-agent","working_dir":"`+dir+`","owner":"ansible"}]`), 0o600))

	reg := newComposeRegistry(path, root)
	must(t, reg.load())

	entry, _ := reg.get("gdrive-agent")
	if entry.Owner != "ansible" {
		t.Errorf("the index lost the real owner to an unreadable mark: %q", entry.Owner)
	}
	if got := markOf(t, dir); got == unknownOwner {
		t.Error("the sentinel was written into the stack directory as a declaration")
	}
	var persisted []ProjectEntry
	raw, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(raw, &persisted))
	for _, p := range persisted {
		if p.Owner == unknownOwner {
			t.Errorf("the sentinel was persisted into the index: %+v", p)
		}
	}
	// And it is still reported, because an owner the directory cannot back up is
	// exactly what this readiness field is for.
	if got := reg.unmarkedOwners(); len(got) != 1 || got[0] != "gdrive-agent" {
		t.Errorf("unmarkedOwners = %v, want [gdrive-agent] — an unreadable mark is not a healthy one", got)
	}
}

// TestAnUnresolvableRootDoesNotRewriteEveryOwner is the same defect at its worst
// trigger: one failing read per entry, all at once, on a boot that then persists.
func TestAnUnresolvableRootDoesNotRewriteEveryOwner(t *testing.T) {
	root := t.TempDir()
	var entries []string
	for _, name := range []string{"gdrive-agent", "duplicacy-agent-api", "plain"} {
		dir := filepath.Join(root, name)
		must(t, os.MkdirAll(dir, 0o755))
		owner := "ansible"
		if name == "plain" {
			owner = "" // an UNOWNED stack must not acquire an owner either
		}
		must(t, writeOwnerMark(dir, root, owner))
		entries = append(entries, `{"name":"`+name+`","working_dir":"`+dir+`","owner":"`+owner+`"}`)
	}
	path := filepath.Join(root, "projects.json")
	must(t, os.WriteFile(path, []byte("["+strings.Join(entries, ",")+"]"), 0o600))

	// Every mark now unreadable, by the same cause, at once.
	for _, name := range []string{"gdrive-agent", "duplicacy-agent-api", "plain"} {
		mark := filepath.Join(root, name, ownerMarkName)
		if _, err := os.Stat(mark); err == nil {
			must(t, os.Remove(mark))
		}
		must(t, os.MkdirAll(mark, 0o755)) // a directory: open succeeds, read does not
	}

	reg := newComposeRegistry(path, root)
	must(t, reg.load())

	for _, e := range reg.list() {
		if e.Owner == unknownOwner {
			t.Errorf("%s was rewritten as owned by the sentinel", e.Name)
		}
	}
	if e, _ := reg.get("plain"); e.Owner != "" {
		t.Errorf("an unowned stack acquired owner %q from a failed read", e.Owner)
	}
	if e, _ := reg.get("gdrive-agent"); e.Owner != "ansible" {
		t.Errorf("a real owner was replaced by a failed read: %q", e.Owner)
	}
}

// TestTheSentinelCannotBeDeclared. It is the one value that must never be
// storable, so it cannot be an input either — otherwise the thing the fix stops
// the agent writing could be written through the front door, and "unknown" on
// the wire would mean two unrelated things.
func TestTheSentinelCannotBeDeclared(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := e.writeCompose(t, "plain", composeA)
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "plain", "working_dir": dir, "replace": true, "owner": unknownOwner,
	})
	if status != http.StatusBadRequest || body["code"] != "invalid_owner" {
		t.Fatalf("register owner=%q: %d %v, want 400 invalid_owner", unknownOwner, status, body)
	}
	if err := writeOwnerMark(dir, e.root, unknownOwner); err == nil {
		t.Error("writeOwnerMark accepted the sentinel")
	}
}

// TestTheReservedNameIsReservedAsAPathComponent. Reproduced against the first
// version: writeProjectFiles MkdirAll's a path's parents before confining it, so
// a bundle path of "<mark>/x" created a DIRECTORY at the reserved name. It then
// read as owner "unknown" (EISDIR), and neither the clear (ENOTEMPTY) nor the
// atomic rename (EEXIST) could remove it — the stack was un-editable,
// un-registerable and unrecoverable through the API, from three unauthenticated
// calls.
func TestTheReservedNameIsReservedAsAPathComponent(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	for _, path := range []string{
		ownerMarkName + "/x", "nested/" + ownerMarkName + "/x", ownerMarkName + "/a/b",
	} {
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
			"name": "forged", "replace": true,
			"files": map[string]string{"docker-compose.yml": composeA, path: "boom"},
		})
		if status != http.StatusBadRequest || body["code"] != "invalid_file_path" {
			t.Errorf("bundle path %q: %d %v, want 400 invalid_file_path", path, status, body)
		}
		if _, err := os.Stat(filepath.Join(e.root, "forged", ownerMarkName)); err == nil {
			t.Fatalf("bundle path %q created the reserved path", path)
		}
	}
}

// TestANonRegularMarkIsRepairedRatherThanWedging is the second half of the same
// finding: the guard above stops it being created through the API, and this stops
// anything already at that path — from an older agent, a restore, a host-side
// tool — being a dead end. Every route out used to fail.
func TestANonRegularMarkIsRepairedRatherThanWedging(t *testing.T) {
	for _, shape := range []string{"non-empty-directory", "symlink"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "stack")
			must(t, os.MkdirAll(dir, 0o755))
			mark := filepath.Join(dir, ownerMarkName)
			if shape == "symlink" {
				other := filepath.Join(root, "elsewhere")
				must(t, os.WriteFile(other, []byte("do not clobber\n"), 0o600))
				must(t, os.Symlink(other, mark))
			} else {
				must(t, os.MkdirAll(mark, 0o755))
				must(t, os.WriteFile(filepath.Join(mark, "x"), []byte("boom"), 0o644))
			}

			owner, err := readOwnerMark(dir, root)
			if err == nil || owner != unknownOwner {
				t.Fatalf("readOwnerMark = %q, %v; want the fail-safe value and an error", owner, err)
			}
			// Both repairs must work, or the stack is stuck.
			must(t, writeOwnerMark(dir, root, "ansible"))
			if got := markOf(t, dir); got != "ansible" {
				t.Fatalf("after the repair the mark reads %q", got)
			}
			must(t, writeOwnerMark(dir, root, ""))
			if got := markOf(t, dir); got != "" {
				t.Errorf("the mark survived a clear: %q", got)
			}
			if shape == "symlink" {
				b, rerr := os.ReadFile(filepath.Join(root, "elsewhere"))
				must(t, rerr)
				if string(b) != "do not clobber\n" {
					t.Error("the write followed the symlink and clobbered its target")
				}
			}
		})
	}
}

// TestAMovedProjectDoesNotOrphanItsOldMark. A mark with no entry is invisible to
// readiness, which only walks entries — and it refuses a later stack registered
// at that path as rendered by a tool that no longer renders it.
func TestAMovedProjectDoesNotOrphanItsOldMark(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	oldDir := registerOwned(t, e, "traefik", "ansible")
	newDir := e.writeCompose(t, "traefik-v2", composeA)

	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "traefik", "working_dir": newDir, "replace": true, "owner": "ansible",
	})
	if status != http.StatusOK {
		t.Fatalf("move: %d %v", status, body)
	}
	if got := markOf(t, newDir); got != "ansible" {
		t.Errorf("the new directory does not record the owner: %q", got)
	}
	if got := markOf(t, oldDir); got != "" {
		t.Errorf("the old directory kept an orphan mark: %q", got)
	}
	// The consequence, stated directly: a fresh stack at the vacated path.
	status, body = e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "traefik-fresh", "replace": true,
		"files": map[string]string{"docker-compose.yml": composeA},
	})
	if status != http.StatusOK {
		t.Fatalf("a new stack at the vacated path: %d %v", status, body)
	}
}

// TestReadinessIsComputedWhenThereIsNoIndexAtALL. load() returned early for a
// missing or empty projects.json — before the reconcile and before the readiness
// refresh — so the phase's own scenario was the one case where readiness
// reported "none" for "not yet checked".
func TestReadinessIsComputedWhenThereIsNoIndexAtAll(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "projects.json")
	reg := newComposeRegistry(path, root)
	must(t, reg.load()) // no file at all
	if reg.unmarked.Load() == nil {
		t.Error("the readiness cache was never computed with no index file")
	}
	must(t, os.WriteFile(path, nil, 0o600))
	reg = newComposeRegistry(path, root)
	must(t, reg.load()) // zero-length file
	if reg.unmarked.Load() == nil {
		t.Error("the readiness cache was never computed with an empty index file")
	}
}

// TestAFifoAtTheMarkPathDoesNotHangTheRead. The mark read runs on the fleet
// snapshot's path. A FIFO there would block the open FOREVER without O_NONBLOCK,
// and the snapshot builds on every liveness tick and every WS connect.
//
// This replaces a test that asserted a register could complete "while a snapshot
// was mid-flight" — it created no FIFO, blocked on nothing, and would have passed
// against any implementation. It proved nothing, which is the thing a
// verification pass exists to find.
func TestAFifoAtTheMarkPathDoesNotHangTheRead(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stack")
	must(t, os.MkdirAll(dir, 0o755))
	if err := syscall.Mkfifo(filepath.Join(dir, ownerMarkName), 0o600); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		owner, err := readOwnerMark(dir, root)
		if err == nil || owner != unknownOwner {
			t.Errorf("a fifo mark read as %q, %v", owner, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the mark read hung on a fifo")
	}
}

// TestAdoptionDoesNotPersistAnUnreadableOwner. Adoption is the other writer, and
// it persists what it builds — so a transient read failure at boot would store
// the sentinel there too, feeding the same migration that turns it into a real
// declaration at the next boot.
func TestAdoptionDoesNotPersistAnUnreadableOwner(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gdrive-agent")
	must(t, os.MkdirAll(filepath.Join(dir, ownerMarkName), 0o755)) // unreadable
	path := filepath.Join(root, "projects.json")
	reg := newComposeRegistry(path, root)
	must(t, reg.load())

	reg.enrichFromLive([]ComposeProject{liveOf("gdrive-agent", dir)})

	if e, ok := reg.get("gdrive-agent"); ok && e.Owner == unknownOwner {
		t.Errorf("adoption stored the sentinel: %+v", e)
	}
	if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), unknownOwner) {
		t.Errorf("the sentinel reached projects.json: %s", raw)
	}
}

// TestARecordedOwnerIsNotRewrittenOnEveryRegister. An owner's converge
// re-registers on every run; rewriting the mark each time is two fsyncs and a new
// inode per stack, which on an SD-card host is the dominant cost of a converge.
func TestARecordedOwnerIsNotRewrittenOnEveryRegister(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stack")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, writeOwnerMark(dir, root, "ansible"))
	first, err := os.Stat(filepath.Join(dir, ownerMarkName))
	must(t, err)

	must(t, writeOwnerMark(dir, root, "ansible"))
	again, err := os.Stat(filepath.Join(dir, ownerMarkName))
	must(t, err)

	if !os.SameFile(first, again) {
		t.Error("re-recording the same owner replaced the file")
	}
	// ...and a DIFFERENT owner still lands.
	must(t, writeOwnerMark(dir, root, "someone-else"))
	if got := markOf(t, dir); got != "someone-else" {
		t.Errorf("a changed owner was not written: %q", got)
	}
}

// TestAMarkSymlinkedWithinTheRootIsRefused. The out-of-root case is caught by
// confinePath; this is the one only O_NOFOLLOW catches — a link to a perfectly
// valid file INSIDE the root, which confinement is happy with.
func TestAMarkSymlinkedWithinTheRootIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "stack")
	must(t, os.MkdirAll(dir, 0o755))
	other := filepath.Join(root, "other-owner")
	must(t, os.WriteFile(other, []byte("ansible\n"), 0o600))
	must(t, os.Symlink(other, filepath.Join(dir, ownerMarkName)))

	owner, err := readOwnerMark(dir, root)
	if err == nil {
		t.Fatalf("a symlinked mark inside the root was read as %q", owner)
	}
	if owner != unknownOwner {
		t.Errorf("owner = %q, want %q", owner, unknownOwner)
	}
}

// TestASnapshotKeepsAnIndexedOwnerWhenTheMarkIsMissing. The snapshot reads the
// directory ONLY for rows the index does not hold. Reading it for every row
// looks harmless — the answers usually agree — but it silently drops a recorded
// owner whose mark has not been written yet or could not be read, and it puts a
// file read per project on the path that builds on every liveness tick.
//
// A canary found this: reading the mark for indexed rows too left every other
// test green.
func TestASnapshotKeepsAnIndexedOwnerWhenTheMarkIsMissing(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gdrive-agent")
	must(t, os.MkdirAll(dir, 0o755))
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)
	must(t, reg.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: dir, Owner: "ansible"}))
	must(t, os.Remove(filepath.Join(dir, ownerMarkName))) // as a failed mark write leaves it

	merged := reg.mergeKnown([]ComposeProject{liveOf("gdrive-agent", dir)}, nil)
	if merged[0].Owner != "ansible" {
		t.Errorf("the snapshot dropped the index's owner: %+v", merged[0])
	}
	if merged[0].Managed {
		t.Error("and offered Edit on it")
	}
}

// TestAnEntryIsImmutableOnceIndexed. persist() collects the *ProjectEntry
// pointers under a read lock, RELEASES it, and marshals them with no lock held —
// which is only safe because an indexed entry is never written through again.
// setOwner broke that by assigning in place. Today its one caller runs before the
// listener binds, so nothing races it; this pins the invariant rather than the
// caller, because the next caller will not know.
func TestAnEntryIsImmutableOnceIndexed(t *testing.T) {
	root := t.TempDir()
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)
	must(t, reg.register(ProjectEntry{Name: "s", WorkingDir: filepath.Join(root, "s")}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = reg.persist()
		}
	}()
	for i := 0; i < 200; i++ {
		reg.setOwner("s", "ansible")
		reg.setOwner("s", "")
	}
	<-done
}
