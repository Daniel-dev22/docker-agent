package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
)

// --- name validity: compose is the authority ---

func TestValidProjectNameAgreesWithCompose(t *testing.T) {
	for _, name := range []string{
		"", "a", "traefik", "duplicacy-agent-api", "stack_1", "0day", "Docker-Agent", "my.stack",
		"-lead", "_lead", "a-", "a_", "UPPER", "sp ace", "tab\t", "ünïcode", "İstanbul", "a/b", "..", "ǅ",
	} {
		want := name != "" && loader.NormalizeProjectName(name) == name
		if got := validProjectName(name); got != want {
			t.Errorf("validProjectName(%q) = %v, compose says %v", name, got, want)
		}
	}
}

func FuzzValidProjectNameAgreesWithCompose(f *testing.F) {
	for _, seed := range []string{"traefik", "Docker-Agent", "my.stack", "-x", "_x", "ß", "\x00", "a-b_c9"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		want := name != "" && loader.NormalizeProjectName(name) == name
		if got := validProjectName(name); got != want {
			t.Fatalf("validProjectName(%q) = %v, compose says %v", name, got, want)
		}
	})
}

// --- the published view ---

func TestObserveKeepsTheNewestView(t *testing.T) {
	s := &selfIdentity{containerID: testSelfID, controlPathName: "traefik"}
	t0 := time.Now()
	newer := defaultContainers("/data")
	older := newer[:1] // the older list had no traefik

	s.observe(summariesOf(newer), t0.Add(2*time.Second))
	got := s.observe(summariesOf(older), t0.Add(time.Second))
	if got.controlPath != nil {
		t.Fatal("observe must return the view derived from ITS list")
	}
	if cur := s.current(); cur.controlPath == nil || !cur.observedAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("a slower, older list replaced the newer view: %+v", cur)
	}

	t.Run("failure-ordering", func(t *testing.T) {
		s.observeFailed(errors.New("older failure"), t0.Add(time.Second))
		if _, has := s.status()["last_list_error"]; has {
			t.Fatal("a failure older than the published view must not be reported")
		}
		s.observeFailed(errors.New("daemon wedged"), t0.Add(3*time.Second))
		if s.status()["last_list_error"] != "daemon wedged" {
			t.Fatalf("status %v", s.status())
		}
		s.observe(summariesOf(newer), t0.Add(2500*time.Millisecond))
		if s.status()["last_list_error"] != "daemon wedged" {
			t.Fatal("a success that started before the failure must not clear it")
		}
		s.observe(summariesOf(newer), t0.Add(4*time.Second))
		if _, has := s.status()["last_list_error"]; has {
			t.Fatalf("a later success must clear the failure: %v", s.status())
		}
	})
}

func TestBootEnrichmentPublishesSelf(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	e.a.enrichProjectsOnce(context.Background())
	v := e.a.self.current()
	if v == nil || !v.found || !v.isSelfProject("docker-agent") {
		t.Fatalf("boot enrichment must publish self identity, got %+v", v)
	}
	if _, ok := e.a.projects.get("gdrive-agent"); !ok {
		t.Fatal("boot enrichment must still adopt running projects")
	}
}

// --- mutations never act on a stale or unbounded view ---

func TestMutationsNeverFallBackToAPriorView(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	e.writeCompose(t, "owned", "services: {}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned"), ComposeFiles: []string{"docker-compose.yml"}}))
	if _, _, err := e.a.observeNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := e.a.self.current(); v == nil || !v.found {
		t.Fatal("precondition: a good view is published")
	}
	e.eng.set(func(f *fakeEngine) { f.listErr = true })
	for _, rq := range []struct {
		name, method, path string
		body               any
	}{
		{"compose-op", http.MethodPost, "/v1/projects/owned/op", map[string]any{"op": "restart"}},
		{"register", http.MethodPost, "/v1/projects", map[string]any{"name": "new", "files": map[string]string{"docker-compose.yml": "services: {}\n"}}},
		{"copy", http.MethodPost, "/v1/projects/owned/copy", map[string]any{"new_name": "owned-copy"}},
		{"container-start", http.MethodPost, "/v1/containers/gdrive-agent/start", nil},
		{"container-remove", http.MethodDelete, "/v1/containers/gdrive-agent", nil},
		{"bulk", http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "stop", "ids": []string{"gdrive-agent"}}},
	} {
		t.Run(rq.name, func(t *testing.T) {
			status, body := e.do(t, rq.method, rq.path, rq.body)
			if status != http.StatusServiceUnavailable || body["code"] != "self_identity_unavailable" {
				t.Fatalf("got %d %v, want 503 even with a prior good view", status, body)
			}
		})
	}
	if n := e.eng.mutationCount(); n != 0 {
		t.Fatalf("engine saw %d mutations", n)
	}
}

func TestMutationListIsBounded(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	prev := selfListTimeout
	selfListTimeout = 150 * time.Millisecond
	t.Cleanup(func() { selfListTimeout = prev })
	hang := make(chan struct{})
	e.eng.set(func(f *fakeEngine) { f.listHang = hang })
	t.Cleanup(func() { close(hang) })

	type result struct {
		status int
		body   map[string]any
	}
	done := make(chan result, 1)
	go func() {
		status, body := e.do(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", nil)
		done <- result{status, body}
	}()
	select {
	case r := <-done:
		if r.status != http.StatusServiceUnavailable || r.body["code"] != "self_identity_unavailable" {
			t.Fatalf("got %d %v", r.status, r.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a hung container list held the request past its bound")
	}
}

func TestGetProjectsRejectsAViewThatNeverFoundSelf(t *testing.T) {
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer { return defaultContainers(root)[1:] })
	if _, _, err := e.a.observeNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := e.a.self.current(); v == nil || v.found {
		t.Fatal("precondition: the published view did not find the agent")
	}
	e.eng.set(func(f *fakeEngine) { f.listErr = true })
	status, body := e.do(t, http.MethodGet, "/v1/projects", nil)
	if status != http.StatusServiceUnavailable || body["code"] != "self_identity_unavailable" {
		t.Fatalf("got %d %v", status, body)
	}
}

// TestControlPathHeldWithoutMountinfo: protecting the control path needs only the
// list, not the agent's own container ID.
func TestControlPathHeldWithoutMountinfo(t *testing.T) {
	e := newCapEnv(t, "", defaultContainers)
	status, body := e.do(t, http.MethodPost, "/v1/containers/traefik/stop", nil)
	if status != http.StatusConflict || body["code"] != "control_path_container" {
		t.Fatalf("got %d %v", status, body)
	}
	must(t, e.a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: e.writeCompose(t, "traefik", "services: {}\n")}))
	status, body = e.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "down"})
	if status != http.StatusConflict || body["code"] != "project_not_operable" {
		t.Fatalf("down on the control-path stack: got %d %v", status, body)
	}
	e.eng.set(func(f *fakeEngine) { f.listErr = true })
	status, body = e.do(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("list failure with a control path to protect: got %d %v", status, body)
	}
	if n := e.eng.mutationCount(); n != 0 {
		t.Fatalf("engine saw %d mutations", n)
	}
}

// --- container targets ---

func TestContainerJobsActOnCheckedIDs(t *testing.T) {
	selfID := "db" + testSelfID[2:]
	dbID := "dbe0" + strings.Repeat("0", 60)
	env := func(t *testing.T) *capEnv {
		return newCapEnv(t, selfID, func(root string) []fakeContainer {
			return []fakeContainer{
				{id: selfID, name: "docker-agent", project: "docker-agent"},
				{id: dbID, name: "db", project: "postgres"},
			}
		})
	}

	t.Run("duplicate-reference-removed-once", func(t *testing.T) {
		e := env(t)
		status, body := e.do(t, http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "remove", "ids": []string{"db", "db"}})
		if status != http.StatusAccepted || body["count"] != float64(1) {
			t.Fatalf("got %d %v", status, body)
		}
		e.waitJobs(t)
		if got := e.eng.mutationList(); len(got) != 1 || got[0] != "DELETE /containers/"+dbID {
			t.Fatalf("engine saw %v, want exactly one remove of db's full ID", got)
		}
	})
	t.Run("two-references-to-one-container-collapse", func(t *testing.T) {
		e := env(t)
		status, body := e.do(t, http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "stop", "ids": []string{"db", dbID}})
		if status != http.StatusAccepted || body["count"] != float64(1) {
			t.Fatalf("got %d %v", status, body)
		}
		e.waitJobs(t)
		if got := e.eng.mutationList(); len(got) != 1 || got[0] != "POST /containers/"+dbID+"/stop" {
			t.Fatalf("engine saw %v", got)
		}
	})
	t.Run("single-op-acts-on-full-id", func(t *testing.T) {
		e := env(t)
		status, _ := e.do(t, http.MethodDelete, "/v1/containers/db", nil)
		if status != http.StatusAccepted {
			t.Fatalf("got %d", status)
		}
		e.waitJobs(t)
		// A second request for "db" now resolves by prefix to the agent — and is refused.
		status, body := e.do(t, http.MethodDelete, "/v1/containers/db", nil)
		if status != http.StatusConflict || body["code"] != "self_container" {
			t.Fatalf("second remove: got %d %v", status, body)
		}
		e.waitJobs(t)
		for _, m := range e.eng.mutationList() {
			if strings.Contains(m, selfID) {
				t.Fatalf("the agent was acted on: %v", e.eng.mutationList())
			}
		}
		if got := e.eng.mutationList(); len(got) != 1 || got[0] != "DELETE /containers/"+dbID {
			t.Fatalf("engine saw %v", got)
		}
	})
	t.Run("not-found-acts-on-nothing", func(t *testing.T) {
		e := env(t)
		status, body := e.do(t, http.MethodPost, "/v1/containers/ghost/stop", nil)
		if status != http.StatusAccepted {
			t.Fatalf("got %d %v", status, body)
		}
		e.waitJobs(t)
		if n := e.eng.mutationCount(); n != 0 {
			t.Fatalf("engine saw %v for a target that did not exist", e.eng.mutationList())
		}
		j, _ := e.a.reg.get(body["job_id"].(string))
		if st := j.snapshot(); st.State != JobFailed || !strings.Contains(st.ErrorMsg, "no such container") {
			t.Fatalf("job %+v", st)
		}
	})
}

func TestContainerTargetResolution(t *testing.T) {
	t.Run("ambiguous-prefix-is-400", func(t *testing.T) {
		e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
			return []fakeContainer{
				{id: testSelfID, name: "docker-agent", project: "docker-agent"},
				{id: "ab" + strings.Repeat("1", 62), name: "one"},
				{id: "ab" + strings.Repeat("2", 62), name: "two"},
			}
		})
		status, body := e.do(t, http.MethodPost, "/v1/containers/ab/stop", nil)
		if status != http.StatusBadRequest || body["code"] != "invalid_target" || body["targets"] == nil {
			t.Fatalf("got %d %v, want 400 invalid_target", status, body)
		}
	})
	t.Run("any-targets-inspect-error-fails-the-request", func(t *testing.T) {
		e := newCapEnv(t, testSelfID, defaultContainers)
		e.eng.set(func(f *fakeEngine) { f.inspectErrFor = map[string]bool{"gdrive-agent": true} })
		status, body := e.do(t, http.MethodPost, "/v1/containers/bulk",
			map[string]any{"action": "restart", "ids": []string{"traefik", "gdrive-agent"}})
		if status != http.StatusServiceUnavailable || body["code"] != "self_identity_unavailable" {
			t.Fatalf("got %d %v", status, body)
		}
		if n := len(e.a.reg.list()); n != 0 {
			t.Fatalf("no job may start; registry holds %d", n)
		}
	})
	t.Run("inspect-concurrency-is-bounded", func(t *testing.T) {
		e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
			cs := defaultContainers(root)
			for i := range 16 {
				cs = append(cs, fakeContainer{id: fmt.Sprintf("%064x", i+1), name: fmt.Sprintf("svc%02d", i)})
			}
			return cs
		})
		e.eng.set(func(f *fakeEngine) { f.inspectDelay = 20 * time.Millisecond })
		ids := make([]string, 0, 16)
		for i := range 16 {
			ids = append(ids, fmt.Sprintf("svc%02d", i))
		}
		status, body := e.do(t, http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "restart", "ids": ids})
		if status != http.StatusAccepted {
			t.Fatalf("got %d %v", status, body)
		}
		e.eng.mu.Lock()
		peak := e.eng.maxInFlight
		e.eng.mu.Unlock()
		if peak > containerResolveConcurrency || peak < 2 {
			t.Fatalf("peak in-flight inspects %d, want 2..%d", peak, containerResolveConcurrency)
		}
	})
	t.Run("too-many-distinct-targets", func(t *testing.T) {
		e := newCapEnv(t, testSelfID, defaultContainers)
		ids := make([]string, 0, maxContainerTargets+1)
		for i := range maxContainerTargets + 1 {
			ids = append(ids, fmt.Sprintf("c%03d", i))
		}
		status, body := e.do(t, http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "stop", "ids": ids})
		if status != http.StatusBadRequest || body["code"] != "too_many_targets" {
			t.Fatalf("got %d %v", status, body)
		}
		e.eng.mu.Lock()
		lists := e.eng.lists
		e.eng.mu.Unlock()
		if lists != 0 {
			t.Fatalf("an oversized request reached the daemon (%d lists)", lists)
		}
		// Duplicates do not count toward the cap.
		dup := make([]string, 0, maxContainerTargets*2)
		for range maxContainerTargets * 2 {
			dup = append(dup, "gdrive-agent")
		}
		if status, body := e.do(t, http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "stop", "ids": dup}); status != http.StatusAccepted || body["count"] != float64(1) {
			t.Fatalf("duplicates: got %d %v", status, body)
		}
	})
}

func TestSelfThatIsAlsoControlPathContainer(t *testing.T) {
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
		return []fakeContainer{{id: testSelfID, name: "traefik", project: "edge"}}
	})
	status, body := e.do(t, http.MethodPost, "/v1/containers/traefik/restart", nil)
	if status != http.StatusConflict || body["code"] != "self_container" {
		t.Fatalf("got %d %v, want 409 self_container", status, body)
	}
}

// --- refusal text is built from what is actually allowed ---

func TestRefusalTextNamesTheAllowedOps(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: e.writeCompose(t, "traefik", "services: {}\n")}))
	_, body := e.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "down"})
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "control-path") || !strings.HasSuffix(msg, "Allowed: pull, recreate, restart, up, update.") {
		t.Fatalf("control-path down: %q", msg)
	}

	e2 := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e2.a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: "/srv/containers/traefik"}))
	for _, op := range []string{"up", "update"} {
		_, body := e2.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": op})
		msg, _ := body["error"].(string)
		if strings.Contains(msg, "control-path") || !strings.Contains(msg, "outside docker-agent's compose root") ||
			!strings.HasSuffix(msg, "Allowed: restart.") {
			t.Fatalf("%s on the outside-root control-path stack: %q", op, msg)
		}
	}
	_, body = e2.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "down"})
	if msg, _ := body["error"].(string); !strings.Contains(msg, "control-path") || !strings.HasSuffix(msg, "Allowed: restart.") {
		t.Fatalf("down on the outside-root control-path stack: %q", msg)
	}
}

// --- register: relocation, declared paths, compose presence ---

func TestRegisterRelocationCannotDeploy(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	// traefik is Ansible-owned outside the root in this scenario.
	e.eng.set(func(f *fakeEngine) { f.containers[1].workingDir = "/srv/containers/traefik" })
	must(t, e.a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: "/srv/containers/traefik"}))

	status, body := e.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "up"})
	if status != http.StatusConflict {
		t.Fatalf("precondition: up in place is refused, got %d %v", status, body)
	}

	status, body = e.do(t, http.MethodPost, "/v1/projects",
		map[string]any{"name": "traefik", "files": map[string]string{"docker-compose.yml": "services: {evil: {}}\n"}, "deploy": true})
	if status != http.StatusConflict || body["code"] != "project_relocation_requires_separate_up" || body["working_dir"] != "/srv/containers/traefik" {
		t.Fatalf("got %d %v", status, body)
	}
	if _, err := os.Stat(filepath.Join(e.root, "traefik")); !os.IsNotExist(err) {
		t.Fatalf("a refused relocation wrote files: %v", err)
	}
	if got, _ := e.a.projects.get("traefik"); got.WorkingDir != "/srv/containers/traefik" {
		t.Fatalf("registry changed: %+v", got)
	}
	if n := len(e.a.reg.list()); n != 0 {
		t.Fatalf("no job may start; registry holds %d", n)
	}

	t.Run("live-label-only-project-counts", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects",
			map[string]any{"name": "gdrive-agent", "files": map[string]string{"docker-compose.yml": "services: {}\n"}, "deploy": true})
		if status != http.StatusConflict || body["code"] != "project_relocation_requires_separate_up" {
			t.Fatalf("got %d %v", status, body)
		}
	})

	t.Run("same-dir-reregister-with-deploy-still-works", func(t *testing.T) {
		dir := e.writeCompose(t, "stable", "services: {}\n")
		must(t, e.a.projects.register(ProjectEntry{Name: "stable", WorkingDir: dir}))
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "stable", "working_dir": dir + "/", "deploy": true})
		if status != http.StatusOK || body["job_id"] == nil {
			t.Fatalf("got %d %v", status, body)
		}
	})

	t.Run("move-without-deploy-then-up-through-the-gate", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects",
			map[string]any{"name": "traefik", "files": map[string]string{"docker-compose.yml": "services: {}\n"}})
		if status != http.StatusOK {
			t.Fatalf("got %d %v", status, body)
		}
		if got, _ := e.a.projects.get("traefik"); got.WorkingDir != filepath.Join(e.root, "traefik") {
			t.Fatalf("not re-pointed: %+v", got)
		}
		// Now under the root: up is allowed; down stays refused (control path).
		if status, body := e.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "up"}); status != http.StatusAccepted {
			t.Fatalf("up after the move: %d %v", status, body)
		}
		if status, _ := e.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "down"}); status != http.StatusConflict {
			t.Fatalf("down after the move: %d", status)
		}
	})
}

func TestRegisterPathChecks(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	dir := e.writeCompose(t, "multi", "services: {}\n")
	must(t, os.WriteFile(filepath.Join(dir, "override.yml"), []byte("services: {}\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "a-directory.yml"), 0o755))
	outside := filepath.Join(t.TempDir(), "bearer-token")
	must(t, os.WriteFile(outside, []byte("SECRET-TOKEN"), 0o600))
	must(t, os.Symlink(outside, filepath.Join(dir, "linked.env")))

	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{"declared-directory-is-not-a-compose-file", map[string]any{"name": "d1", "working_dir": dir, "compose_files": []string{"a-directory.yml"}}, http.StatusBadRequest, ""},
		{"every-declared-file-must-exist", map[string]any{"name": "d2", "working_dir": dir, "compose_files": []string{"docker-compose.yml", "missing.yml"}}, http.StatusBadRequest, ""},
		{"env-file-escaping-the-working-dir", map[string]any{"name": "d3", "working_dir": dir, "env_files": []string{"/etc/docker-agent/bearer-token"}}, http.StatusBadRequest, "project_path_outside_working_dir"},
		{"relative-escape", map[string]any{"name": "d4", "working_dir": dir, "compose_files": []string{"../other/docker-compose.yml"}}, http.StatusBadRequest, "project_path_outside_working_dir"},
		{"symlink-escaping-the-working-dir", map[string]any{"name": "d5", "working_dir": dir, "env_files": []string{"linked.env"}}, http.StatusBadRequest, "project_path_outside_working_dir"},
		{"inline-files-declaring-an-outside-env", map[string]any{"name": "d6", "files": map[string]string{"docker-compose.yml": "services: {}\n"}, "env_files": []string{"/etc/passwd"}}, http.StatusBadRequest, "project_path_outside_working_dir"},
		{"relative-working-dir", map[string]any{"name": "d7", "working_dir": "relative/dir"}, http.StatusBadRequest, ""},
		{"relative-compose-files-resolve-in-the-working-dir", map[string]any{"name": "ok1", "working_dir": dir, "compose_files": []string{"docker-compose.yml", "override.yml"}}, http.StatusOK, ""},
		{"outside-root-visibility-register-keeps-its-paths", map[string]any{"name": "ok2", "working_dir": "/srv/containers/ok2", "compose_files": []string{"/srv/containers/ok2/docker-compose.yml"}}, http.StatusOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := e.do(t, http.MethodPost, "/v1/projects", c.body)
			if status != c.wantStatus || (c.wantCode != "" && body["code"] != c.wantCode) {
				t.Fatalf("got %d %v, want %d %s", status, body, c.wantStatus, c.wantCode)
			}
			_, registered := e.a.projects.get(c.body["name"].(string))
			if registered != (c.wantStatus == http.StatusOK) {
				t.Fatalf("registered=%v after %d", registered, status)
			}
		})
	}
}

// --- bundle: file content only for editable projects, only under the root ---

func TestBundleReadsOnlyConfinedEditableFiles(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	token := filepath.Join(t.TempDir(), "bearer-token")
	must(t, os.WriteFile(token, []byte("SECRET-TOKEN"), 0o600))

	dir := e.writeCompose(t, "owned", "services: {}\n")
	must(t, os.Symlink(token, filepath.Join(dir, ".env")))
	must(t, os.Symlink(token, filepath.Join(dir, "evil.yml")))
	// Entries written before register validated paths (or edited by hand).
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: dir,
		ComposeFiles: []string{"docker-compose.yml", "evil.yml"}, EnvFiles: []string{token}}))
	must(t, e.a.projects.register(ProjectEntry{Name: "external", WorkingDir: filepath.Dir(token), EnvFiles: []string{token}}))

	get := func(name string) (int, string, projectBundle) {
		req := httptest.NewRequest(http.MethodGet, "/v1/projects/"+name+"/bundle", nil)
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		var b projectBundle
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		return w.Code, w.Body.String(), b
	}

	status, raw, b := get("owned")
	if status != http.StatusOK || strings.Contains(raw, "SECRET-TOKEN") {
		t.Fatalf("editable bundle leaked a file outside the root: %d %s", status, raw)
	}
	if len(b.ComposeFiles) != 1 || b.ComposeFiles[0].Name != "docker-compose.yml" || len(b.EnvFiles) != 0 {
		t.Fatalf("only the confined compose file may be read: %+v", b)
	}

	status, raw, b = get("external")
	if status != http.StatusOK || strings.Contains(raw, "SECRET-TOKEN") {
		t.Fatalf("non-editable bundle leaked content: %d %s", status, raw)
	}
	if !strings.Contains(raw, `"compose_files":[]`) || !strings.Contains(raw, `"env_files":[]`) || b.WorkingDir != filepath.Dir(token) {
		t.Fatalf("non-editable bundle must keep the typed empty shape: %s", raw)
	}

	t.Run("copy-reads-through-the-same-confinement", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects/owned/copy", map[string]any{"new_name": "owned-copy"})
		if status != http.StatusOK {
			t.Fatalf("got %d %v", status, body)
		}
		entries, _ := os.ReadDir(filepath.Join(e.root, "owned-copy"))
		for _, de := range entries {
			data, _ := os.ReadFile(filepath.Join(e.root, "owned-copy", de.Name()))
			if strings.Contains(string(data), "SECRET-TOKEN") {
				t.Fatalf("copy wrote the outside file into %s", de.Name())
			}
		}
	})

	t.Run("loading-the-project-refuses-escaping-files", func(t *testing.T) {
		entry, _ := e.a.projects.get("owned")
		if err := confineProjectFiles(entry, e.root); err == nil || !strings.Contains(err.Error(), "outside docker-agent's compose root") {
			t.Fatalf("got %v", err)
		}
		clean := ProjectEntry{Name: "clean", WorkingDir: e.writeCompose(t, "clean", "services: {}\n")}
		if err := confineProjectFiles(clean, e.root); err != nil {
			t.Fatalf("a confined project must load: %v", err)
		}
		// A discovered (undeclared) compose file that is a symlink out of the root.
		disc := filepath.Join(e.root, "disc")
		must(t, os.MkdirAll(disc, 0o755))
		must(t, os.Symlink(token, filepath.Join(disc, "compose.yaml")))
		if err := confineProjectFiles(ProjectEntry{Name: "disc", WorkingDir: disc}, e.root); err == nil {
			t.Fatal("a discovered compose file resolving outside the root must refuse the load")
		}
		cb := &composeBackend{root: e.root}
		if _, err := cb.loadProject(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("loadProject must apply the confinement first: %v", err)
		}
	})
}

func TestReadinessControlPathError(t *testing.T) {
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer { return defaultContainers(root)[:1] })
	if _, _, err := e.a.observeNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, http.MethodGet, "/health/ready", nil)
	self, _ := body["self"].(map[string]any)
	if self["control_path_error"] != `no container named "traefik"` || self["control_path"] != nil {
		t.Fatalf("readiness self: %v", self)
	}
}
