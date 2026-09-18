package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// An externally-rendered stack: Ansible writes the files under the compose root,
// the agent operates them. These tests pin the join between the three things that
// have to agree — what is persisted, what is advertised, and what is refused.

// registerOwned is the call the Ansible role makes: path-only, replace, owner.
func registerOwned(t *testing.T, e *capEnv, name, owner string) string {
	t.Helper()
	dir := e.writeCompose(t, name, composeA)
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": name, "working_dir": dir, "replace": true, "owner": owner,
	})
	if status != http.StatusOK {
		t.Fatalf("register %s owner=%q: %d %v", name, owner, status, body)
	}
	return dir
}

// TestOwnedStackIsOperableButNotEditable is the phase's headline claim: the four
// Ansible-rendered agent stacks move under the compose root to become operable,
// and ownership is what stops that also making them editable.
func TestOwnedStackIsOperableButNotEditable(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	registerOwned(t, e, "gdrive-agent", "ansible")

	entry, ok := e.a.projects.get("gdrive-agent")
	if !ok || entry.Owner != "ansible" {
		t.Fatalf("owner not persisted: %+v", entry)
	}

	status, body := e.do(t, http.MethodGet, "/v1/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, body)
	}
	row := listedProject(t, body, "gdrive-agent")
	if row["owner"] != "ansible" {
		t.Errorf("owner missing from the list row: %v", row)
	}
	if row["operable"] != true {
		t.Errorf("an owned stack under the root must stay operable: %v", row)
	}
	if row["managed"] != false {
		t.Errorf("an owned stack must not be editable: %v", row)
	}
	// No op is blocked, so nothing may claim one is: ops_blocked is about ops.
	if _, present := row["ops_blocked"]; present {
		t.Errorf("ops_blocked must be absent for a fully operable stack: %v", row)
	}
	ops, _ := row["allowed_ops"].([]any)
	if len(ops) != len(ALL_OPS_SORTED) {
		t.Errorf("allowed_ops = %v, want every op", ops)
	}
}

// ALL_OPS_SORTED is the full op set, as capability sorts it.
var ALL_OPS_SORTED = []string{"down", "pull", "recreate", "restart", "up", "update"}

// TestOwnedStackRefusesEveryFileWrite: the flag has to be load-bearing on every
// path that reads or rewrites the files, or it is decoration.
func TestOwnedStackRefusesEveryFileWrite(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "gdrive-agent", "ansible")
	before, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	must(t, err)

	t.Run("editor-save-is-refused", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
			"name": "gdrive-agent", "replace": true,
			"files": map[string]string{"docker-compose.yml": "services:\n  app:\n    image: evil:latest\n"},
		})
		if status != http.StatusConflict || body["code"] != "project_owned" {
			t.Fatalf("register-with-files onto an owned stack: %d %v", status, body)
		}
		if body["owner"] != "ansible" {
			t.Errorf("the refusal must name the owner: %v", body)
		}
		after, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
		must(t, err)
		if string(after) != string(before) {
			t.Fatal("a refused register rewrote the owner's compose file")
		}
	})

	t.Run("bundle-is-empty-so-no-secret-is-served", func(t *testing.T) {
		// The four agent stacks keep bearer tokens beside their compose. Under the
		// root, only the editable flag keeps /bundle from serving them.
		must(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=secret-value\n"), 0o600))
		status, body := e.do(t, http.MethodGet, "/v1/projects/gdrive-agent/bundle", nil)
		if status != http.StatusOK {
			t.Fatalf("bundle: %d %v", status, body)
		}
		raw, err := json.Marshal(body)
		must(t, err)
		if len(raw) > 0 && containsStr(string(raw), "secret-value") {
			t.Fatalf("an owned stack's bundle served its env file: %s", raw)
		}
		if files, _ := body["compose_files"].([]any); len(files) != 0 {
			t.Errorf("compose_files = %v, want empty", files)
		}
	})

	t.Run("the-owner-may-push-its-own-files", func(t *testing.T) {
		// Ansible's own render-and-push path (ups/deploy_nut_stack.yaml) says who it
		// is. Saying so is the difference between the owner converging its stack and
		// the editor overwriting it.
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
			"name": "gdrive-agent", "replace": true, "owner": "ansible",
			"files": map[string]string{"docker-compose.yml": composeA},
		})
		if status != http.StatusOK {
			t.Fatalf("owner pushing its own files: %d %v", status, body)
		}
		if entry, _ := e.a.projects.get("gdrive-agent"); entry.Owner != "ansible" {
			t.Fatalf("the owner was lost by its own push: %+v", entry)
		}
		// A DIFFERENT owner is still refused: two renderers on one stack is the
		// conflict the flag exists to name.
		status, body = e.do(t, http.MethodPost, "/v1/projects", map[string]any{
			"name": "gdrive-agent", "replace": true, "owner": "terraform",
			"files": map[string]string{"docker-compose.yml": composeA},
		})
		if status != http.StatusConflict || body["code"] != "project_owned" {
			t.Fatalf("a different owner pushing files: %d %v", status, body)
		}
	})

	t.Run("copy-source-is-refused", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects/gdrive-agent/copy", map[string]any{"new_name": "gdrive-copy"})
		if status != http.StatusConflict || body["code"] != "project_owned" {
			t.Fatalf("copy from an owned stack: %d %v", status, body)
		}
	})
}

// TestOwnerIsPreservedUnlessExplicitlyCleared. Absent and empty differ: a
// re-register by anything that does not know about ownership — a remediation, a
// migration — must not silently unclaim an Ansible stack and reopen the editor.
func TestOwnerIsPreservedUnlessExplicitlyCleared(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := registerOwned(t, e, "build-agent", "ansible")

	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "build-agent", "working_dir": dir, "replace": true,
	})
	if status != http.StatusOK {
		t.Fatalf("re-register without owner: %d %v", status, body)
	}
	if entry, _ := e.a.projects.get("build-agent"); entry.Owner != "ansible" {
		t.Fatalf("an owner-less re-register cleared the owner: %+v", entry)
	}

	status, body = e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "build-agent", "working_dir": dir, "replace": true, "owner": "",
	})
	if status != http.StatusOK {
		t.Fatalf("explicit clear: %d %v", status, body)
	}
	entry, _ := e.a.projects.get("build-agent")
	if entry.Owner != "" {
		t.Fatalf(`"owner": "" must clear it: %+v`, entry)
	}
	// And the editor reopens, which is the point of being able to clear it.
	if !e.a.capabilityOf(entry, e.a.self.current()).Editable {
		t.Error("a cleared stack is editable again")
	}
}

// TestOwnerSurvivesARestart: the flag is only worth having if it is durable —
// projects.json is what the agent reloads, and an owner lost on restart reopens
// the editor on files Ansible renders.
func TestOwnerSurvivesARestart(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	registerOwned(t, e, "filemesh-agent", "ansible")

	reloaded := newComposeRegistry(e.a.projects.path, e.root)
	must(t, reloaded.load())
	entry, ok := reloaded.get("filemesh-agent")
	if !ok || entry.Owner != "ansible" {
		t.Fatalf("owner did not survive a reload of projects.json: %+v", entry)
	}
}

// TestInvalidOwnerIsRefused: the value lands in projects.json and in refusal
// text, so it is bounded and coded like every other deterministic 4xx.
func TestInvalidOwnerIsRefused(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := e.writeCompose(t, "stack", composeA)

	for _, owner := range []string{"Ansible", "ansible ", "an/sible", "a\nb", string(make([]byte, maxOwnerLen+1))} {
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
			"name": "stack", "working_dir": dir, "replace": true, "owner": owner,
		})
		if status != http.StatusBadRequest || body["code"] != "invalid_owner" {
			t.Errorf("owner %q: %d %v", owner, status, body)
		}
	}
	if _, ok := e.a.projects.get("stack"); ok {
		t.Error("a refused register was recorded")
	}
}

// TestOwnedRegisterStillChecksTheComposeFileIsThere. The presence check used to
// be gated on "editable", which ownership now turns off — an owned register must
// not become the one path that can claim operable with nothing to load.
func TestOwnedRegisterStillChecksTheComposeFileIsThere(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := filepath.Join(e.root, "empty-agent")
	must(t, os.MkdirAll(dir, 0o755))
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{
		"name": "empty-agent", "working_dir": dir, "replace": true, "owner": "ansible",
	})
	if status != http.StatusBadRequest || body["code"] != "compose_files_missing" {
		t.Fatalf("owned register with no compose file: %d %v", status, body)
	}
}

func containsStr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// listedProject pulls one row out of a GET /v1/projects body.
func listedProject(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	rows, _ := body["projects"].([]any)
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("project %s not in %v", name, body)
	return nil
}

// TestOwnerOnTheFleetSnapshotWire. The list row gets `owner` from the embedded
// registry entry, so it cannot tell whether the FLEET snapshot carries it — and
// the snapshot is what the frontend reads to explain a locked editor. This goes
// through mergeKnown, the assembler between the registry and the wire.
func TestOwnerOnTheFleetSnapshotWire(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	registerOwned(t, e, "gdrive-agent", "ansible")

	view := e.a.self.current()
	merged := e.a.projects.mergeKnown(nil, view)
	var got *ComposeProject
	for i := range merged {
		if merged[i].Name == "gdrive-agent" {
			got = &merged[i]
		}
	}
	if got == nil {
		t.Fatalf("gdrive-agent missing from the snapshot: %+v", merged)
	}
	raw, err := json.Marshal(got)
	must(t, err)
	for _, want := range []string{`"owner":"ansible"`, `"managed":false`, `"operable":true`} {
		if !containsStr(string(raw), want) {
			t.Errorf("snapshot wire %s missing %s", raw, want)
		}
	}

	// A project with no owner must not put an empty owner on the wire: absent is
	// how an older agent reads, and a consumer tells them apart.
	agent := e.writeCompose(t, "frigate", composeA)
	status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "frigate", "working_dir": agent})
	if status != http.StatusOK {
		t.Fatalf("register frigate: %d %v", status, body)
	}
	merged = e.a.projects.mergeKnown(nil, view)
	for i := range merged {
		if merged[i].Name != "frigate" {
			continue
		}
		raw, err := json.Marshal(merged[i])
		must(t, err)
		if containsStr(string(raw), "owner") {
			t.Errorf("an unowned stack must omit owner: %s", raw)
		}
	}
}
