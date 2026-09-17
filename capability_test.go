package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const testSelfID = "3f1c9a7be2d04c6f8a51b0e9d7c2a4f61e8b3d05c9a7f2e4b6d8c0a1f3e5b7d9"

func mountinfoLine(root, mountPoint string) string {
	return "812 790 259:2 " + root + " " + mountPoint + " rw,relatime - ext4 /dev/nvme0n1p2 rw"
}

func TestParseSelfContainerID(t *testing.T) {
	const other = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	overlayRoot := "790 1 0:45 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/ABC"
	cases := []struct {
		name    string
		lines   []string
		want    string
		wantErr bool
	}{
		{
			name: "standard-data-root",
			lines: []string{
				overlayRoot,
				mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/resolv.conf", "/etc/resolv.conf"),
				mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/hostname", "/etc/hostname"),
				mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/hosts", "/etc/hosts"),
			},
			want: testSelfID,
		},
		{
			name:  "relocated-data-root",
			lines: []string{mountinfoLine("/mnt/.ix-apps/docker/containers/"+testSelfID+"/hostname", "/etc/hostname")},
			want:  testSelfID,
		},
		{
			// An operator bind mount whose host path looks like a container dir is
			// not identity: only Docker's own setup mounts are trusted.
			name: "lookalike-bind-mount-ignored",
			lines: []string{
				mountinfoLine("/srv/containers/"+other+"/data", "/data"),
				mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/hosts", "/etc/hosts"),
			},
			want: testSelfID,
		},
		{name: "no-docker-mounts", lines: []string{overlayRoot, mountinfoLine("/", "/etc/hostname")}, wantErr: true},
		{name: "empty", lines: nil, wantErr: true},
		{name: "malformed-short-lines", lines: []string{"garbage", "1 2 3", ""}, wantErr: true},
		{name: "uppercase-hex-rejected", lines: []string{mountinfoLine("/var/lib/docker/containers/"+strings.ToUpper(testSelfID)+"/hostname", "/etc/hostname")}, wantErr: true},
		{
			name: "two-containers-is-an-error",
			lines: []string{
				mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/hostname", "/etc/hostname"),
				mountinfoLine("/var/lib/docker/containers/"+other+"/hosts", "/etc/hosts"),
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSelfContainerID(strings.NewReader(strings.Join(c.lines, "\n")))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got id %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got (%q, %v), want %q", got, err, c.want)
			}
		})
	}
}

func TestProjectCapabilities(t *testing.T) {
	root := "/srv/containers/docker-agent/data"
	cases := []struct {
		name, project, dir, self string
		want                     projectCapability
	}{
		{"under-root", "traefik", root + "/traefik", "docker-agent", projectCapability{Operable: true, Editable: true}},
		{"outside-root", "gdrive-agent", "/srv/containers/gdrive-agent", "docker-agent", projectCapability{Blocked: blockedOutsideComposeRoot}},
		{"self-outside-root", "docker-agent", "/srv/containers/docker-agent", "docker-agent", projectCapability{Blocked: blockedSelf}},
		// The guard must not depend on where the agent's compose file lives.
		{"self-INSIDE-root", "docker-agent", root + "/docker-agent", "docker-agent", projectCapability{Blocked: blockedSelf}},
		{"self-unknown-falls-back-to-root-rule", "docker-agent", "/srv/containers/docker-agent", "", projectCapability{Blocked: blockedOutsideComposeRoot}},
		{"no-working-dir", "adhoc", "", "docker-agent", projectCapability{Blocked: blockedOutsideComposeRoot}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := projectCapabilities(c.project, c.dir, root, c.self); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestComposeProjectWireCarriesFalse pins the JOIN between the agent and every
// consumer: `false` must be ON the wire. With omitempty it was dropped, and every
// downstream `managed === false` check was unreachable while each side's own tests
// passed.
func TestComposeProjectWireCarriesFalse(t *testing.T) {
	blocked := ComposeProject{Name: "gdrive-agent"}
	blocked.stampCapability(projectCapabilities("gdrive-agent", "/elsewhere/gdrive-agent", "/data", "docker-agent"))
	raw, err := json.Marshal(blocked)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"managed":false`, `"operable":false`, `"ops_blocked":"outside_compose_root"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("wire %s missing %s", raw, want)
		}
	}

	ok := ComposeProject{Name: "traefik"}
	ok.stampCapability(projectCapabilities("traefik", "/data/traefik", "/data", "docker-agent"))
	raw, _ = json.Marshal(ok)
	if !bytes.Contains(raw, []byte(`"managed":true`)) || !bytes.Contains(raw, []byte(`"operable":true`)) {
		t.Errorf("operable wire %s missing true flags", raw)
	}
	if bytes.Contains(raw, []byte(`ops_blocked`)) {
		t.Errorf("operable wire %s must omit ops_blocked", raw)
	}
}

// TestMergeKnownStampsEveryProject covers all three origins of a snapshot row.
func TestMergeKnownStampsEveryProject(t *testing.T) {
	root := t.TempDir()
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)
	must(t, reg.register(ProjectEntry{Name: "stopped-external", WorkingDir: "/elsewhere/stack"}))
	must(t, reg.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(root, "owned")}))
	// Registered under the root, but the live containers still carry a pre-migration
	// label path: the registered path is what an op loads, so it decides.
	must(t, reg.register(ProjectEntry{Name: "migrated", WorkingDir: filepath.Join(root, "migrated")}))

	live := []ComposeProject{
		{Name: "owned", WorkingDir: filepath.Join(root, "owned"), RunningCount: 1},
		{Name: "migrated", WorkingDir: "/data/compose/49/v3", RunningCount: 1},
		{Name: "adhoc-external", WorkingDir: "/tmp/adhoc", RunningCount: 1},
		{Name: "docker-agent", WorkingDir: filepath.Join(root, "docker-agent"), RunningCount: 1},
	}
	out := reg.mergeKnown(live, "docker-agent")

	want := map[string]projectCapability{
		"owned":            {Operable: true, Editable: true},
		"migrated":         {Operable: true, Editable: true},
		"stopped-external": {Blocked: blockedOutsideComposeRoot},
		"adhoc-external":   {Blocked: blockedOutsideComposeRoot},
		"docker-agent":     {Blocked: blockedSelf},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d projects, want %d", len(out), len(want))
	}
	for _, p := range out {
		got := projectCapability{Operable: p.Operable, Editable: p.Managed, Blocked: p.OpsBlocked}
		if got != want[p.Name] {
			t.Errorf("%s: got %+v, want %+v", p.Name, got, want[p.Name])
		}
	}
}

// --- handler tests ---

type fakeInspector struct {
	byRef map[string][3]string // ref → {id, name, project}
	err   error
	calls int
}

func (f *fakeInspector) containerIdentity(_ context.Context, ref string) (string, string, map[string]string, error) {
	f.calls++
	if f.err != nil {
		return "", "", nil, f.err
	}
	v, ok := f.byRef[ref]
	if !ok {
		return "", "", nil, errors.New("no such container: " + ref)
	}
	return v[0], v[1], map[string]string{labelComposeProject: v[2]}, nil
}

func resolvedSelf(t *testing.T) *selfIdentity {
	t.Helper()
	s := &selfIdentity{
		containerID: testSelfID,
		inspector:   &fakeInspector{byRef: map[string][3]string{testSelfID: {testSelfID, "/docker-agent", "docker-agent"}}},
	}
	if got := s.projectName(context.Background()); got != "docker-agent" {
		t.Fatalf("self project = %q", got)
	}
	return s
}

func newCapabilityTestApp(t *testing.T) (*gin.Engine, *app) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	cfg := Config{ComposeRoot: root, ComposeRegistryPath: filepath.Join(root, "projects.json")}
	reg := newComposeRegistry(cfg.ComposeRegistryPath, root)
	// engine with no docker/compose: a job that does start fails gracefully.
	jobs := newJobRegistry(cfg, newEngine(cfg, nil, nil, reg), nil)
	a := &app{cfg: cfg, projects: reg, reg: jobs, compose: &composeBackend{}, self: resolvedSelf(t)}
	r := gin.New()
	registerRoutes(r, a)
	return r, a
}

func doJSON(t *testing.T, r http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		must(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestComposeOpRefusals(t *testing.T) {
	r, a := newCapabilityTestApp(t)
	root := a.cfg.ComposeRoot
	must(t, a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/elsewhere/gdrive-agent"}))
	must(t, a.projects.register(ProjectEntry{Name: "docker-agent", WorkingDir: filepath.Join(root, "docker-agent")}))
	must(t, a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: filepath.Join(root, "traefik")}))

	cases := []struct {
		name, project, op string
		wantStatus        int
		wantCode          string
	}{
		{"outside-root-update", "gdrive-agent", "update", http.StatusConflict, "project_not_operable"},
		{"outside-root-down", "gdrive-agent", "down", http.StatusConflict, "project_not_operable"},
		{"self-inside-root-update", "docker-agent", "update", http.StatusConflict, "self_project"},
		{"self-restart", "docker-agent", "restart", http.StatusConflict, "self_project"},
		{"unknown", "nope", "up", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := doJSON(t, r, http.MethodPost, "/v1/projects/"+c.project+"/op", map[string]any{"op": c.op})
			if status != c.wantStatus {
				t.Fatalf("status %d, want %d (%v)", status, c.wantStatus, body)
			}
			if c.wantCode != "" && body["code"] != c.wantCode {
				t.Fatalf("code %v, want %s (%v)", body["code"], c.wantCode, body)
			}
			if n := len(a.reg.list()); n != 0 {
				t.Fatalf("a refusal must create no job; registry holds %d", n)
			}
		})
	}

	t.Run("operable-accepted", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "up"})
		if status != http.StatusAccepted || body["job_id"] == "" {
			t.Fatalf("status %d body %v, want 202 + job_id", status, body)
		}
		if n := len(a.reg.list()); n != 1 {
			t.Fatalf("expected exactly one job, got %d", n)
		}
	})
}

func TestCopyAndRegisterRefusals(t *testing.T) {
	r, a := newCapabilityTestApp(t)
	root := a.cfg.ComposeRoot
	must(t, a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/elsewhere/gdrive-agent"}))
	ownedDir := filepath.Join(root, "owned")
	must(t, os.MkdirAll(ownedDir, 0o755))
	must(t, os.WriteFile(filepath.Join(ownedDir, "docker-compose.yml"), []byte("services: {}\n"), 0o644))
	must(t, a.projects.register(ProjectEntry{Name: "owned", WorkingDir: ownedDir, ComposeFiles: []string{"docker-compose.yml"}}))

	cases := []struct {
		name, method, path string
		body               map[string]any
		wantCode           string
		mustNotRegister    string
	}{
		{"copy-non-editable-source", http.MethodPost, "/v1/projects/gdrive-agent/copy", map[string]any{"new_name": "gdrive-copy"}, "project_not_editable", "gdrive-copy"},
		{"copy-onto-self-name", http.MethodPost, "/v1/projects/owned/copy", map[string]any{"new_name": "docker-agent", "deploy": true}, "self_project", "docker-agent"},
		{"register-self-name", http.MethodPost, "/v1/projects", map[string]any{"name": "docker-agent", "files": map[string]string{"docker-compose.yml": "services: {}\n"}}, "self_project", "docker-agent"},
		{"register-outside-root-with-deploy", http.MethodPost, "/v1/projects", map[string]any{"name": "ext", "working_dir": "/elsewhere/ext", "deploy": true}, "project_not_operable", "ext"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := doJSON(t, r, c.method, c.path, c.body)
			if status != http.StatusConflict || body["code"] != c.wantCode {
				t.Fatalf("got %d %v, want 409 %s", status, body, c.wantCode)
			}
			if _, ok := a.projects.get(c.mustNotRegister); ok {
				t.Fatalf("%s was registered despite the refusal", c.mustNotRegister)
			}
			if n := len(a.reg.list()); n != 0 {
				t.Fatalf("a refusal must create no job; registry holds %d", n)
			}
		})
	}

	t.Run("copy-editable-source-accepted", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodPost, "/v1/projects/owned/copy", map[string]any{"new_name": "owned-copy"})
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
	})
}

func TestContainerVerbsRefuseSelf(t *testing.T) {
	r, a := newCapabilityTestApp(t)
	for _, ref := range []string{testSelfID, testSelfID[:12], "docker-agent", "/docker-agent"} {
		for _, verb := range []string{"start", "stop", "restart"} {
			t.Run(verb+"/"+ref, func(t *testing.T) {
				status, body := doJSON(t, r, http.MethodPost, "/v1/containers/"+strings.TrimPrefix(ref, "/")+"/"+verb, nil)
				if status != http.StatusConflict || body["code"] != "self_container" {
					t.Fatalf("got %d %v, want 409 self_container", status, body)
				}
			})
		}
	}
	t.Run("remove", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodDelete, "/v1/containers/"+testSelfID[:12], nil)
		if status != http.StatusConflict || body["code"] != "self_container" {
			t.Fatalf("got %d %v, want 409 self_container", status, body)
		}
	})
	t.Run("bulk-containing-self-refused-whole", func(t *testing.T) {
		status, body := doJSON(t, r, http.MethodPost, "/v1/containers/bulk",
			map[string]any{"action": "restart", "ids": []string{"0123456789ab", testSelfID[:12], "frigate"}})
		if status != http.StatusConflict || body["code"] != "self_container" {
			t.Fatalf("got %d %v, want 409 self_container", status, body)
		}
	})
	if n := len(a.reg.list()); n != 0 {
		t.Fatalf("refusals must create no job; registry holds %d", n)
	}
}

func TestIsSelfContainer(t *testing.T) {
	ctx := context.Background()
	s := resolvedSelf(t)
	cases := map[string]bool{
		testSelfID:        true,
		testSelfID[:12]:   true,
		"docker-agent":    true,
		"/docker-agent":   true,
		"0123456789ab":    false,
		"frigate":         false,
		"docker-agent-v2": false,
		"":                false,
	}
	for ref, want := range cases {
		if got := s.isSelfContainer(ctx, ref); got != want {
			t.Errorf("isSelfContainer(%q) = %v, want %v", ref, got, want)
		}
	}

	t.Run("name-unknown-asks-the-daemon", func(t *testing.T) {
		// Project resolution failed, so the agent's own name is unknown: a name
		// target is resolved by inspecting it.
		insp := &fakeInspector{byRef: map[string][3]string{
			"renamed-agent": {testSelfID, "/renamed-agent", "docker-agent"},
			"frigate":       {strings.Repeat("b", 64), "/frigate", "frigate"},
		}}
		u := &selfIdentity{containerID: testSelfID, inspector: insp}
		if !u.isSelfContainer(ctx, "renamed-agent") {
			t.Error("a name resolving to the agent's ID must be self")
		}
		if u.isSelfContainer(ctx, "frigate") {
			t.Error("a name resolving elsewhere must not be self")
		}
	})

	t.Run("no-identity-never-self", func(t *testing.T) {
		u := &selfIdentity{idErr: errors.New("not in docker")}
		if u.isSelfContainer(ctx, testSelfID) {
			t.Error("an agent with no identity cannot match anything")
		}
	})
}

func TestSelfProjectRetryIsThrottled(t *testing.T) {
	insp := &fakeInspector{err: errors.New("daemon unreachable")}
	s := &selfIdentity{containerID: testSelfID, inspector: insp}
	for range 5 {
		if got := s.projectName(context.Background()); got != "" {
			t.Fatalf("got %q from a failing daemon", got)
		}
	}
	if insp.calls != 1 {
		t.Fatalf("inspected %d times inside one retry window, want 1", insp.calls)
	}
	st := s.status()
	if st["container_id"] != testSelfID[:12] || !strings.Contains(st["error"].(string), "daemon unreachable") {
		t.Fatalf("status %v must name the container and the failure", st)
	}
}

func TestReadinessReportsSelfWithoutGating(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &app{self: &selfIdentity{idErr: errors.New("no docker container mounts in mountinfo")}}
	r := gin.New()
	r.GET("/health/ready", a.handleReadiness)
	status, body := doJSON(t, r, http.MethodGet, "/health/ready", nil)
	if status != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("readiness must stay 200/ready, got %d %v", status, body)
	}
	self, _ := body["self"].(map[string]any)
	if self == nil || !strings.Contains(self["error"].(string), "mountinfo") {
		t.Fatalf("readiness must surface the self-identity failure, got %v", body)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
