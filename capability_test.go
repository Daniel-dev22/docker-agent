package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/gin-gonic/gin"
)

const (
	testSelfID    = "3f1c9a7be2d04c6f8a51b0e9d7c2a4f61e8b3d05c9a7f2e4b6d8c0a1f3e5b7d9"
	testTraefikID = "7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a7a"
	testGdriveID  = "9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b"
)

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

// --- a fake Docker Engine API, so the production client, list adapter and
// handlers all run for real ---

type fakeContainer struct {
	id, name, project, workingDir, networkMode string
}

type fakeEngine struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	containers []fakeContainer
	listErr    bool
	listHang   chan struct{} // non-nil: the list blocks until closed or the request ends
	inspectErr bool
	// inspectErrFor fails only these references.
	inspectErrFor map[string]bool
	inspectDelay  time.Duration
	inFlight      int
	maxInFlight   int
	lists         int
	mutations     []string
}

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeEngine(t *testing.T, containers ...fakeContainer) *fakeEngine {
	t.Helper()
	f := &fakeEngine{t: t, containers: containers}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEngine) host() string { return "tcp://" + strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeEngine) set(fn func(f *fakeEngine)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeEngine) mutationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mutations)
}

func (f *fakeEngine) mutationList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mutations...)
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	path := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		w.Header().Set("API-Version", "1.47")
		w.Header().Set("OSType", "linux")
		_, _ = w.Write([]byte("OK"))
	case path == "/version":
		writeJSON(http.StatusOK, map[string]string{"Version": "28.5.2", "ApiVersion": "1.47", "MinAPIVersion": "1.24"})
	case r.Method == http.MethodGet && path == "/containers/json":
		f.mu.Lock()
		f.lists++
		hang, fail := f.listHang, f.listErr
		cs := append([]fakeContainer(nil), f.containers...)
		f.mu.Unlock()
		if hang != nil {
			select {
			case <-hang:
			case <-r.Context().Done():
				return
			}
		}
		if fail {
			writeJSON(http.StatusInternalServerError, map[string]string{"message": "daemon wedged"})
			return
		}
		out := make([]container.Summary, 0, len(cs))
		for _, c := range cs {
			s := container.Summary{
				ID: c.id, Names: []string{"/" + c.name}, State: "running", Status: "Up 1 hour",
				Labels: map[string]string{},
			}
			if c.project != "" {
				s.Labels[labelComposeProject] = c.project
				s.Labels[labelComposeWorkingDir] = c.workingDir
			}
			s.HostConfig.NetworkMode = c.networkMode
			// The real list endpoint returns Aliases/DNSNames null on every network
			// (verified against Docker 29.7.2), so the fake never lists them.
			s.NetworkSettings = &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{
				"traefik-network": {},
			}}
			out = append(out, s)
		}
		writeJSON(http.StatusOK, out)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		f.mu.Lock()
		fail := f.inspectErr || f.inspectErrFor[ref]
		delay := f.inspectDelay
		cs := append([]fakeContainer(nil), f.containers...)
		f.inFlight++
		if f.inFlight > f.maxInFlight {
			f.maxInFlight = f.inFlight
		}
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
		}()
		if delay > 0 {
			time.Sleep(delay)
		}
		if fail {
			writeJSON(http.StatusInternalServerError, map[string]string{"message": "inspect timed out"})
			return
		}
		// The daemon's order: exact ID, exact name, unique ID prefix.
		var hit *fakeContainer
		for i := range cs {
			if cs[i].id == ref {
				hit = &cs[i]
			}
		}
		if hit == nil {
			for i := range cs {
				if cs[i].name == strings.TrimPrefix(ref, "/") {
					hit = &cs[i]
				}
			}
		}
		if hit == nil {
			var matches []*fakeContainer
			for i := range cs {
				if strings.HasPrefix(cs[i].id, ref) {
					matches = append(matches, &cs[i])
				}
			}
			if len(matches) == 1 {
				hit = matches[0]
			}
			if len(matches) > 1 {
				// The daemon answers an ambiguous prefix with InvalidParameter.
				writeJSON(http.StatusBadRequest, map[string]string{"message": "multiple IDs found with provided prefix: " + ref})
				return
			}
		}
		if hit == nil {
			writeJSON(http.StatusNotFound, map[string]string{"message": "No such container: " + ref})
			return
		}
		writeJSON(http.StatusOK, container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: hit.id, Name: "/" + hit.name},
			Config:            &container.Config{Labels: map[string]string{labelComposeProject: hit.project}},
		})
	case strings.HasPrefix(path, "/containers/"):
		f.mu.Lock()
		f.mutations = append(f.mutations, r.Method+" "+path)
		// A removed container is gone: a later reference to it resolves elsewhere.
		if r.Method == http.MethodDelete {
			ref := strings.TrimPrefix(path, "/containers/")
			kept := f.containers[:0]
			for _, c := range f.containers {
				if c.id != ref && c.name != ref {
					kept = append(kept, c)
				}
			}
			f.containers = kept
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(http.StatusNotFound, map[string]string{"message": "unexpected " + r.Method + " " + path})
	}
}

// capEnv is a test app wired like newApp's handlers: a real dockerClient against
// the fake engine, a real registry and job registry.
type capEnv struct {
	r    *gin.Engine
	a    *app
	eng  *fakeEngine
	root string
}

func defaultContainers(root string) []fakeContainer {
	return []fakeContainer{
		{id: testSelfID, name: "docker-agent", project: "docker-agent", workingDir: "/srv/containers/docker-agent"},
		{id: testTraefikID, name: "traefik", project: "traefik", workingDir: filepath.Join(root, "traefik")},
		{id: testGdriveID, name: "gdrive-agent", project: "gdrive-agent", workingDir: "/srv/containers/gdrive-agent"},
	}
}

func newCapEnv(t *testing.T, selfID string, containers func(root string) []fakeContainer) *capEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	eng := newFakeEngine(t, containers(root)...)
	dc, err := newDockerClient(eng.host())
	must(t, err)
	t.Cleanup(func() { _ = dc.close() })
	cfg := Config{ComposeRoot: root, ComposeRegistryPath: filepath.Join(root, "projects.json"), TraefikDockerDNS: "traefik"}
	reg := newComposeRegistry(cfg.ComposeRegistryPath, root)
	// engine with the real client but no compose backend: a compose job fails
	// gracefully, a container job reaches the fake engine.
	jobs := newJobRegistry(cfg, newEngine(cfg, dc, nil, reg), nil)
	self := &selfIdentity{containerID: selfID, controlPathName: "traefik"}
	a := &app{cfg: cfg, docker: dc, projects: reg, reg: jobs, compose: &composeBackend{}, self: self}
	r := gin.New()
	registerRoutes(r, a)
	env := &capEnv{r: r, a: a, eng: eng, root: root}
	t.Cleanup(func() { env.waitJobs(t) })
	return env
}

func (e *capEnv) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	return doJSON(t, e.r, method, path, body)
}

// waitJobs lets every started job reach a terminal state, so none outlives the
// fake engine.
func (e *capEnv) waitJobs(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending := false
		for _, j := range e.a.reg.list() {
			if j.State == JobPending || j.State == JobRunning {
				pending = true
			}
		}
		if !pending {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("jobs still running after 5s")
}

func (e *capEnv) writeCompose(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(e.root, name)
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(content), 0o644))
	return dir
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

func summariesOf(cs []fakeContainer) []container.Summary {
	var summaries []container.Summary
	for _, c := range cs {
		s := container.Summary{ID: c.id, Names: []string{"/" + c.name}, Labels: map[string]string{labelComposeProject: c.project}}
		s.HostConfig.NetworkMode = c.networkMode
		summaries = append(summaries, s)
	}
	return summaries
}

func observed(t *testing.T, selfID string, cs []fakeContainer) *selfView {
	t.Helper()
	return deriveSelfView(selfID, "traefik", summariesOf(cs), time.Now())
}

// --- the capability rule ---

func TestDeriveSelfView(t *testing.T) {
	const dep = "5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d"
	base := defaultContainers("/data")

	t.Run("own-container-and-control-path", func(t *testing.T) {
		v := observed(t, testSelfID, base)
		if !v.found || v.name != "docker-agent" || !v.isSelfProject("docker-agent") || !v.isSelfID(testSelfID) {
			t.Fatalf("own container not derived: %+v", v)
		}
		if v.isSelfProject("traefik") || len(v.ambiguous) != 0 {
			t.Fatalf("only the agent is self: %+v", v)
		}
		if !v.isControlPathID(testTraefikID) || !v.isControlPathProject("traefik") {
			t.Fatalf("control path not derived: %+v", v.controlPath)
		}
	})
	t.Run("network-namespace-dependents-are-self", func(t *testing.T) {
		for _, mode := range []string{"container:" + testSelfID, "container:docker-agent"} {
			cs := append(append([]fakeContainer{}, base...),
				fakeContainer{id: dep, name: "sidecar", project: "sidecar-stack", networkMode: mode})
			v := observed(t, testSelfID, cs)
			if !v.isSelfID(dep) || !v.isSelfProject("sidecar-stack") || !reflect.DeepEqual(v.ambiguous, []string{"sidecar"}) {
				t.Fatalf("mode %q: dependent not treated as self: %+v", mode, v)
			}
		}
	})
	t.Run("own-container-missing-keeps-id", func(t *testing.T) {
		v := observed(t, testSelfID, base[1:])
		if v.found || !v.isSelfID(testSelfID) || v.isSelfProject("docker-agent") {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("control-path-is-the-exact-name-only", func(t *testing.T) {
		cs := []fakeContainer{{id: testTraefikID, name: "traefik-proxy", project: "edge"}}
		v := observed(t, testSelfID, cs)
		if v.controlPath != nil || v.controlPathErr != `no container named "traefik"` {
			t.Fatalf("a near name must not match: %+v / %q", v.controlPath, v.controlPathErr)
		}
	})
	t.Run("control-path-missing-is-reported", func(t *testing.T) {
		v := observed(t, testSelfID, base[:1])
		if v.controlPath != nil || !strings.Contains(v.controlPathErr, "traefik") {
			t.Fatalf("got %+v / %q", v.controlPath, v.controlPathErr)
		}
	})
	t.Run("other-network-namespace-owners-are-not-self", func(t *testing.T) {
		const other = "6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e6e"
		cs := append(append([]fakeContainer{}, base...),
			fakeContainer{id: other, name: "vpn-client", project: "vpn", networkMode: "container:" + testGdriveID},
			fakeContainer{id: strings.Repeat("4", 64), name: "tailnet", project: "tailnet", networkMode: "container:gdrive-agent"})
		v := observed(t, testSelfID, cs)
		if v.isSelfID(other) || v.isSelfProject("vpn") || v.isSelfProject("tailnet") || len(v.ambiguous) != 0 {
			t.Fatalf("only namespaces owned by the agent are self: %+v", v)
		}
	})
	t.Run("no-mountinfo-id-still-derives-control-path", func(t *testing.T) {
		v := observed(t, "", base)
		if len(v.ids) != 0 || v.isSelfProject("docker-agent") || !v.isControlPathProject("traefik") {
			t.Fatalf("got %+v", v)
		}
	})
}

func TestProjectCapabilities(t *testing.T) {
	root := "/srv/containers/docker-agent/data"
	v := observed(t, testSelfID, []fakeContainer{
		{id: testSelfID, name: "docker-agent", project: "docker-agent"},
		{id: testTraefikID, name: "traefik", project: "traefik"},
	})
	full := []string{"down", "pull", "recreate", "restart", "up", "update"}
	cases := []struct {
		name, project, dir string
		view               *selfView
		allowed            []string
		editable           bool
		blocked            string
	}{
		{"under-root", "frigate", root + "/frigate", v, full, true, ""},
		{"outside-root-keeps-label-ops", "gdrive-agent", "/srv/containers/gdrive-agent", v, []string{"down", "restart"}, false, blockedOutsideComposeRoot},
		{"no-working-dir", "adhoc", "", v, []string{"down", "restart"}, false, blockedOutsideComposeRoot},
		{"self-outside-root", "docker-agent", "/srv/containers/docker-agent", v, []string{}, false, blockedSelf},
		// The guard must not depend on where the agent's compose file lives.
		{"self-INSIDE-root", "docker-agent", root + "/docker-agent", v, []string{}, false, blockedSelf},
		// compose lowercases before Down/Restart: "Docker-Agent" would act on the agent.
		{"case-alias-of-self", "Docker-Agent", root + "/Docker-Agent", v, []string{}, false, blockedInvalidName},
		{"dotted-name", "my.stack", root + "/my.stack", v, []string{}, false, blockedInvalidName},
		{"control-path-under-root", "traefik", root + "/traefik", v, []string{"pull", "recreate", "restart", "up", "update"}, true, blockedControlPath},
		{"control-path-outside-root", "traefik", "/srv/containers/traefik", v, []string{"restart"}, false, blockedOutsideComposeRoot},
		// The agent's own stack also runs the control-path proxy: self still wins.
		{"self-that-is-also-control-path", "docker-agent", root + "/docker-agent", observed(t, testSelfID, []fakeContainer{
			{id: testSelfID, name: "docker-agent", project: "docker-agent"},
			{id: testTraefikID, name: "traefik", project: "docker-agent"},
		}), []string{}, false, blockedSelf},
		{"no-view", "docker-agent", root + "/docker-agent", nil, full, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := projectCapabilities(c.project, c.dir, root, c.view)
			if !reflect.DeepEqual(got.Allowed, c.allowed) || got.Editable != c.editable || got.Blocked != c.blocked {
				t.Fatalf("got %+v, want allowed=%v editable=%v blocked=%q", got, c.allowed, c.editable, c.blocked)
			}
			if got.Allowed == nil {
				t.Fatal("Allowed must never be nil")
			}
		})
	}
}

// TestSnapshotWireContract pins the JOIN between the agent and every consumer:
// false and the empty op list must be ON the wire.
func TestSnapshotWireContract(t *testing.T) {
	v := observed(t, testSelfID, defaultContainers("/data"))
	self := ComposeProject{Name: "docker-agent"}
	self.stampCapability(projectCapabilities("docker-agent", "/srv/containers/docker-agent", "/data", v))
	raw, err := json.Marshal(self)
	must(t, err)
	for _, want := range []string{`"allowed_ops":[]`, `"managed":false`, `"operable":false`, `"ops_blocked":"self"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("self wire %s missing %s", raw, want)
		}
	}

	ok := ComposeProject{Name: "frigate"}
	ok.stampCapability(projectCapabilities("frigate", "/data/frigate", "/data", v))
	raw, _ = json.Marshal(ok)
	for _, want := range []string{`"allowed_ops":["down","pull","recreate","restart","up","update"]`, `"managed":true`, `"operable":true`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("operable wire %s missing %s", raw, want)
		}
	}
	if bytes.Contains(raw, []byte(`ops_blocked`)) {
		t.Errorf("fully operable wire %s must omit ops_blocked", raw)
	}

	raw, _ = json.Marshal(ContainerStatus{Name: "frigate"})
	if bytes.Contains(raw, []byte(`"self"`)) || bytes.Contains(raw, []byte(`"control_path"`)) {
		t.Errorf("an ordinary container must omit self/control_path: %s", raw)
	}
}

// TestMergeKnownStampsEveryProject covers all three origins of a snapshot row.
func TestMergeKnownStampsEveryProject(t *testing.T) {
	root := t.TempDir()
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)
	must(t, reg.register(ProjectEntry{Name: "stopped-external", WorkingDir: "/elsewhere/stack"}))
	must(t, reg.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(root, "owned")}))
	// Registered under the root, but the live containers still carry a pre-migration
	// label path: the registered path is what an op loads, so it decides AND it is
	// the working_dir the row reports.
	must(t, reg.register(ProjectEntry{Name: "migrated", WorkingDir: filepath.Join(root, "migrated")}))

	live := []ComposeProject{
		{Name: "owned", WorkingDir: filepath.Join(root, "owned"), RunningCount: 1},
		{Name: "migrated", WorkingDir: "/data/compose/49/v3", RunningCount: 1},
		{Name: "adhoc-external", WorkingDir: "/tmp/adhoc", RunningCount: 1},
		{Name: "docker-agent", WorkingDir: filepath.Join(root, "docker-agent"), RunningCount: 1},
	}
	v := observed(t, testSelfID, []fakeContainer{{id: testSelfID, name: "docker-agent", project: "docker-agent"}})
	out := reg.mergeKnown(live, v)

	want := map[string]struct {
		blocked string
		dir     string
	}{
		"owned":            {"", filepath.Join(root, "owned")},
		"migrated":         {"", filepath.Join(root, "migrated")},
		"stopped-external": {blockedOutsideComposeRoot, "/elsewhere/stack"},
		"adhoc-external":   {blockedOutsideComposeRoot, "/tmp/adhoc"},
		"docker-agent":     {blockedSelf, filepath.Join(root, "docker-agent")},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d projects, want %d", len(out), len(want))
	}
	for _, p := range out {
		w := want[p.Name]
		if p.OpsBlocked != w.blocked || p.WorkingDir != w.dir || p.AllowedOps == nil {
			t.Errorf("%s: got blocked=%q dir=%q ops=%v, want blocked=%q dir=%q", p.Name, p.OpsBlocked, p.WorkingDir, p.AllowedOps, w.blocked, w.dir)
		}
		if p.Operable != (w.blocked == "") || p.Managed != (w.blocked == "") {
			t.Errorf("%s: operable=%v managed=%v inconsistent with blocked=%q", p.Name, p.Operable, p.Managed, w.blocked)
		}
	}
}

// --- handlers, through the real routes and client ---

func TestComposeOpCapabilities(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/srv/containers/gdrive-agent"}))
	must(t, e.a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: e.writeCompose(t, "traefik", "services: {}\n")}))
	// A registry entry for the agent's own project pointing under the root: what a
	// re-register would have left. Self is keyed on the live label, so it stays refused.
	must(t, e.a.projects.register(ProjectEntry{Name: "docker-agent", WorkingDir: filepath.Join(e.root, "docker-agent")}))
	// An entry written before names were validated: compose would act on "docker-agent".
	must(t, e.a.projects.register(ProjectEntry{Name: "Docker-Agent", WorkingDir: filepath.Join(e.root, "Docker-Agent")}))

	cases := []struct {
		name, project, op string
		wantStatus        int
		wantCode          string
	}{
		{"outside-root-update", "gdrive-agent", "update", http.StatusConflict, "project_not_operable"},
		{"outside-root-up", "gdrive-agent", "up", http.StatusConflict, "project_not_operable"},
		{"outside-root-pull", "gdrive-agent", "pull", http.StatusConflict, "project_not_operable"},
		{"outside-root-recreate", "gdrive-agent", "recreate", http.StatusConflict, "project_not_operable"},
		{"self-registered-under-root-update", "docker-agent", "update", http.StatusConflict, "self_project"},
		{"self-restart", "docker-agent", "restart", http.StatusConflict, "self_project"},
		{"self-down", "docker-agent", "down", http.StatusConflict, "self_project"},
		{"case-alias-down", "Docker-Agent", "down", http.StatusConflict, "project_not_operable"},
		{"case-alias-restart", "Docker-Agent", "restart", http.StatusConflict, "project_not_operable"},
		{"control-path-down", "traefik", "down", http.StatusConflict, "project_not_operable"},
		{"unknown", "nope", "up", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := e.do(t, http.MethodPost, "/v1/projects/"+c.project+"/op", map[string]any{"op": c.op})
			if status != c.wantStatus {
				t.Fatalf("status %d, want %d (%v)", status, c.wantStatus, body)
			}
			if c.wantCode != "" {
				if body["code"] != c.wantCode {
					t.Fatalf("code %v, want %s (%v)", body["code"], c.wantCode, body)
				}
				if _, ok := body["working_dir"]; !ok {
					t.Fatalf("a project refusal must carry working_dir: %v", body)
				}
			}
			if n := len(e.a.reg.list()); n != 0 {
				t.Fatalf("a refusal must create no job; registry holds %d", n)
			}
		})
	}
	t.Run("control-path-down-says-why", func(t *testing.T) {
		_, body := e.do(t, http.MethodPost, "/v1/projects/traefik/op", map[string]any{"op": "down"})
		if msg, _ := body["error"].(string); !strings.Contains(msg, "control-path") {
			t.Fatalf("the refusal must name the reason, got %q", msg)
		}
	})

	accepted := []struct{ project, op string }{
		{"gdrive-agent", "restart"}, // compose rebuilds restart/down from labels
		{"gdrive-agent", "down"},
		{"traefik", "restart"},
		{"traefik", "update"},
		{"traefik", "recreate"},
		// Live-only (never registered), outside the root: label ops still run.
		{"frigate-live", "restart"},
	}
	e.eng.set(func(f *fakeEngine) {
		f.containers = append(f.containers, fakeContainer{id: strings.Repeat("c", 64), name: "frigate", project: "frigate-live", workingDir: "/opt/frigate"})
	})
	for _, c := range accepted {
		t.Run("accepted/"+c.project+"/"+c.op, func(t *testing.T) {
			status, body := e.do(t, http.MethodPost, "/v1/projects/"+c.project+"/op", map[string]any{"op": c.op})
			if status != http.StatusAccepted || body["job_id"] == nil {
				t.Fatalf("status %d body %v, want 202 + job_id", status, body)
			}
		})
	}
	t.Run("live-only-outside-root-update-refused-not-404", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects/frigate-live/op", map[string]any{"op": "update"})
		if status != http.StatusConflict || body["code"] != "project_not_operable" {
			t.Fatalf("got %d %v", status, body)
		}
	})
}

func TestRefusalOutranksUnavailableBackend(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	e.a.compose = nil
	must(t, e.a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/srv/containers/gdrive-agent"}))
	must(t, e.a.projects.register(ProjectEntry{Name: "docker-agent", WorkingDir: filepath.Join(e.root, "docker-agent")}))
	for name, code := range map[string]string{"gdrive-agent": "project_not_operable", "docker-agent": "self_project"} {
		status, body := e.do(t, http.MethodPost, "/v1/projects/"+name+"/op", map[string]any{"op": "update"})
		if status != http.StatusConflict || body["code"] != code {
			t.Fatalf("%s: got %d %v, want 409 %s", name, status, body, code)
		}
	}
	// An op the capability allows then meets the unavailable backend.
	status, _ := e.do(t, http.MethodPost, "/v1/projects/gdrive-agent/op", map[string]any{"op": "restart"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("allowed op with no compose backend: got %d, want 503", status)
	}
}

// TestListFailureFailsClosed: the agent knows which container it is but cannot
// see the current list — every mutation is refused as retryable, none allowed blind.
func TestListFailureFailsClosed(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	e.writeCompose(t, "owned", "services: {}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned"), ComposeFiles: []string{"docker-compose.yml"}}))
	must(t, e.a.projects.register(ProjectEntry{Name: "docker-agent", WorkingDir: filepath.Join(e.root, "docker-agent")}))
	e.eng.set(func(f *fakeEngine) { f.listErr = true })

	requests := []struct {
		name, method, path string
		body               any
	}{
		{"compose-op", http.MethodPost, "/v1/projects/owned/op", map[string]any{"op": "restart"}},
		{"register", http.MethodPost, "/v1/projects", map[string]any{"name": "docker-agent", "files": map[string]string{"docker-compose.yml": "services: {}\n"}}},
		{"copy", http.MethodPost, "/v1/projects/owned/copy", map[string]any{"new_name": "owned-copy"}},
		{"container-stop", http.MethodPost, "/v1/containers/docker-agent/stop", nil},
		{"container-remove", http.MethodDelete, "/v1/containers/" + testSelfID[:12], nil},
		{"bulk", http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "restart", "ids": []string{"docker-agent"}}},
		{"list-projects-no-prior-view", http.MethodGet, "/v1/projects", nil},
	}
	for _, rq := range requests {
		t.Run(rq.name, func(t *testing.T) {
			status, body := e.do(t, rq.method, rq.path, rq.body)
			if status != http.StatusServiceUnavailable || body["code"] != "self_identity_unavailable" {
				t.Fatalf("got %d %v, want 503 self_identity_unavailable", status, body)
			}
		})
	}
	if n := len(e.a.reg.list()); n != 0 {
		t.Fatalf("no job may start blind; registry holds %d", n)
	}
	if p, _ := e.a.projects.get("docker-agent"); len(p.ComposeFiles) != 0 {
		t.Fatal("a blind register was persisted")
	}
	if n := e.eng.mutationCount(); n != 0 {
		t.Fatalf("the engine saw %d mutations", n)
	}

	t.Run("list-projects-uses-last-view-of-own-container", func(t *testing.T) {
		e.eng.set(func(f *fakeEngine) { f.listErr = false })
		if status, _ := e.do(t, http.MethodGet, "/v1/projects", nil); status != http.StatusOK {
			t.Fatalf("got %d", status)
		}
		e.eng.set(func(f *fakeEngine) { f.listErr = true })
		req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
		// The stale view must still protect the agent's own stack — not read blind.
		var resp struct {
			Projects []map[string]any `json:"projects"`
		}
		must(t, json.Unmarshal(w.Body.Bytes(), &resp))
		for _, p := range resp.Projects {
			if p["name"] == "docker-agent" && p["ops_blocked"] != "self" {
				t.Fatalf("stale view lost the self guard: %v", p)
			}
		}
	})
}

func TestRegisterAndCopy(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/srv/containers/gdrive-agent"}))
	e.writeCompose(t, "owned", "services: {copied: {}}\n")
	must(t, e.a.projects.register(ProjectEntry{Name: "owned", WorkingDir: filepath.Join(e.root, "owned"), ComposeFiles: []string{"docker-compose.yml"}}))
	// The agent's own compose file under the root — the case a refusal that wrote
	// first would destroy.
	selfFile := filepath.Join(e.writeCompose(t, "docker-agent", "ORIGINAL\n"), "docker-compose.yml")

	refusals := []struct {
		name, path string
		body       map[string]any
		wantStatus int
		wantCode   string
	}{
		{"register-case-alias", "/v1/projects", map[string]any{"name": "Docker-Agent", "files": map[string]string{"docker-compose.yml": "EVIL\n"}}, http.StatusBadRequest, "invalid_project_name"},
		{"copy-onto-case-alias", "/v1/projects/owned/copy", map[string]any{"new_name": "Docker-Agent"}, http.StatusBadRequest, "invalid_project_name"},
		{"register-self-name-inline", "/v1/projects", map[string]any{"name": "docker-agent", "files": map[string]string{"docker-compose.yml": "EVIL\n"}}, http.StatusConflict, "self_project"},
		{"register-self-name-path", "/v1/projects", map[string]any{"name": "docker-agent", "working_dir": filepath.Dir(selfFile)}, http.StatusConflict, "self_project"},
		// Without deploy the deploy gate never runs: the self-name refusal alone must
		// stop the write over the agent's own compose file.
		{"copy-onto-self-name", "/v1/projects/owned/copy", map[string]any{"new_name": "docker-agent"}, http.StatusConflict, "self_project"},
		{"copy-onto-self-name-deploy", "/v1/projects/owned/copy", map[string]any{"new_name": "docker-agent", "deploy": true}, http.StatusConflict, "self_project"},
		{"register-self-name-inline-no-deploy", "/v1/projects", map[string]any{"name": "docker-agent", "files": map[string]string{"docker-compose.yml": "EVIL\n"}, "deploy": false}, http.StatusConflict, "self_project"},
		{"copy-non-editable-source", "/v1/projects/gdrive-agent/copy", map[string]any{"new_name": "gdrive-copy"}, http.StatusConflict, "project_not_editable"},
		{"register-outside-root-with-deploy", "/v1/projects", map[string]any{"name": "ext", "working_dir": "/elsewhere/ext", "deploy": true}, http.StatusConflict, "project_not_operable"},
		{"register-under-root-missing-compose", "/v1/projects", map[string]any{"name": "ghost", "working_dir": filepath.Join(e.root, "ghost")}, http.StatusBadRequest, ""},
	}
	for _, c := range refusals {
		t.Run(c.name, func(t *testing.T) {
			status, body := e.do(t, http.MethodPost, c.path, c.body)
			if status != c.wantStatus || (c.wantCode != "" && body["code"] != c.wantCode) {
				t.Fatalf("got %d %v, want %d %s", status, body, c.wantStatus, c.wantCode)
			}
			if n := len(e.a.reg.list()); n != 0 {
				t.Fatalf("a refusal must create no job; registry holds %d", n)
			}
		})
	}
	t.Run("copy-from-self", func(t *testing.T) {
		must(t, e.a.projects.register(ProjectEntry{Name: "docker-agent", WorkingDir: filepath.Dir(selfFile), ComposeFiles: []string{"docker-compose.yml"}}))
		status, body := e.do(t, http.MethodPost, "/v1/projects/docker-agent/copy", map[string]any{"new_name": "agent-copy"})
		if status != http.StatusConflict || body["code"] != "self_project" {
			t.Fatalf("got %d %v, want 409 self_project", status, body)
		}
	})
	if got, err := os.ReadFile(selfFile); err != nil || string(got) != "ORIGINAL\n" {
		t.Fatalf("the agent's own compose file was rewritten by a refused request: %q %v", got, err)
	}
	for _, name := range []string{"Docker-Agent", "ext", "ghost", "gdrive-copy", "agent-copy"} {
		if _, ok := e.a.projects.get(name); ok {
			t.Errorf("%s was registered despite the refusal", name)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "Docker-Agent")); !os.IsNotExist(err) {
		t.Errorf("a refused register created its directory: %v", err)
	}

	t.Run("input-errors-are-400-before-identity", func(t *testing.T) {
		e.eng.set(func(f *fakeEngine) { f.listErr = true })
		defer e.eng.set(func(f *fakeEngine) { f.listErr = false })
		status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "x", "deploy": true})
		if status != http.StatusBadRequest || strings.Contains(body["error"].(string), "outside") {
			t.Fatalf("got %d %v, want 400 working_dir or files is required", status, body)
		}
	})

	accepted := []struct {
		name string
		body map[string]any
		job  bool
	}{
		{"inline-files-and-deploy", map[string]any{"name": "copied", "files": map[string]string{"docker-compose.yml": "services: {}\n"}, "deploy": true}, true},
		{"path-only-outside-root-visibility", map[string]any{"name": "ansible-owned", "working_dir": "/srv/containers/ansible-owned"}, false},
		{"path-only-under-root-with-compose", map[string]any{"name": "present", "working_dir": e.writeCompose(t, "present", "services: {}\n")}, false},
	}
	for _, c := range accepted {
		t.Run("accepted/"+c.name, func(t *testing.T) {
			status, body := e.do(t, http.MethodPost, "/v1/projects", c.body)
			if status != http.StatusOK {
				t.Fatalf("got %d %v", status, body)
			}
			if c.job && body["job_id"] == nil {
				t.Fatalf("deploy must return a job_id: %v", body)
			}
			if _, ok := e.a.projects.get(c.body["name"].(string)); !ok {
				t.Fatal("not registered")
			}
		})
	}
	t.Run("accepted/copy-editable-source", func(t *testing.T) {
		status, body := e.do(t, http.MethodPost, "/v1/projects/owned/copy", map[string]any{"new_name": "owned-copy"})
		if status != http.StatusOK {
			t.Fatalf("got %d %v", status, body)
		}
	})
}

func TestContainerVerbs(t *testing.T) {
	const depID = "5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d"
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
		return append(defaultContainers(root),
			fakeContainer{id: depID, name: "netshare", networkMode: "container:" + testSelfID})
	})

	refused := []struct {
		name, method, path string
		body               any
		wantCode           string
	}{
		{"self-full-id-stop", http.MethodPost, "/v1/containers/" + testSelfID + "/stop", nil, "self_container"},
		{"self-short-prefix-restart", http.MethodPost, "/v1/containers/" + testSelfID[:4] + "/restart", nil, "self_container"},
		{"self-name-start", http.MethodPost, "/v1/containers/docker-agent/start", nil, "self_container"},
		{"self-remove", http.MethodDelete, "/v1/containers/docker-agent", nil, "self_container"},
		{"netns-dependent-stop", http.MethodPost, "/v1/containers/netshare/stop", nil, "self_container"},
		{"control-path-stop", http.MethodPost, "/v1/containers/traefik/stop", nil, "control_path_container"},
		{"control-path-remove", http.MethodDelete, "/v1/containers/traefik", nil, "control_path_container"},
		{"control-path-bulk-kill", http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "kill", "ids": []string{"gdrive-agent", "traefik"}}, "control_path_container"},
		{"bulk-with-self-refused-whole", http.MethodPost, "/v1/containers/bulk", map[string]any{"action": "restart", "ids": []string{"gdrive-agent", testSelfID[:12]}}, "self_container"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			status, body := e.do(t, c.method, c.path, c.body)
			if status != http.StatusConflict || body["code"] != c.wantCode {
				t.Fatalf("got %d %v, want 409 %s", status, body, c.wantCode)
			}
			if targets, _ := body["targets"].([]any); len(targets) == 0 {
				t.Fatalf("a container refusal must name its targets: %v", body)
			}
		})
	}
	if n := len(e.a.reg.list()); n != 0 {
		t.Fatalf("refusals must create no job; registry holds %d", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := e.eng.mutationCount(); n != 0 {
		t.Fatalf("refusals reached the engine %d times", n)
	}

	t.Run("renamed-self-still-refused", func(t *testing.T) {
		e.eng.set(func(f *fakeEngine) { f.containers[0].name = "renamed-agent" })
		defer e.eng.set(func(f *fakeEngine) { f.containers[0].name = "docker-agent" })
		status, body := e.do(t, http.MethodPost, "/v1/containers/renamed-agent/stop", nil)
		if status != http.StatusConflict || body["code"] != "self_container" {
			t.Fatalf("got %d %v", status, body)
		}
	})

	accepted := []struct{ name, method, path string }{
		{"control-path-restart", http.MethodPost, "/v1/containers/traefik/restart"},
		{"control-path-start", http.MethodPost, "/v1/containers/traefik/start"},
		{"ordinary-stop", http.MethodPost, "/v1/containers/gdrive-agent/stop"},
		// Not found: nobody's to protect; the op fails on its own.
		{"not-found", http.MethodPost, "/v1/containers/ghost/stop"},
	}
	for _, c := range accepted {
		t.Run("accepted/"+c.name, func(t *testing.T) {
			status, body := e.do(t, c.method, c.path, nil)
			if status != http.StatusAccepted || body["job_id"] == nil {
				t.Fatalf("got %d %v, want 202 + job_id", status, body)
			}
		})
	}

	t.Run("inspect-error-is-503", func(t *testing.T) {
		e.eng.set(func(f *fakeEngine) { f.inspectErr = true })
		defer e.eng.set(func(f *fakeEngine) { f.inspectErr = false })
		status, body := e.do(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", nil)
		if status != http.StatusServiceUnavailable || body["code"] != "self_identity_unavailable" || body["targets"] == nil {
			t.Fatalf("got %d %v", status, body)
		}
	})
}

// TestHexNamedContainerIsNotSelf: the daemon resolves an exact name before an ID
// prefix, so a container named "db" is not the agent whose ID starts with "db".
func TestHexNamedContainerIsNotSelf(t *testing.T) {
	selfID := "db" + testSelfID[2:]
	e := newCapEnv(t, selfID, func(root string) []fakeContainer {
		return []fakeContainer{
			{id: selfID, name: "docker-agent", project: "docker-agent"},
			{id: strings.Repeat("e", 64), name: "db", project: "postgres"},
		}
	})
	status, body := e.do(t, http.MethodPost, "/v1/containers/db/stop", nil)
	if status != http.StatusAccepted {
		t.Fatalf("got %d %v, want 202", status, body)
	}
}

func TestListProjectsCarriesCapability(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/srv/containers/gdrive-agent"}))
	must(t, e.a.projects.register(ProjectEntry{Name: "docker-agent", WorkingDir: "/srv/containers/docker-agent"}))
	must(t, e.a.projects.register(ProjectEntry{Name: "traefik", WorkingDir: filepath.Join(e.root, "traefik")}))

	req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	var resp struct {
		Projects []map[string]any `json:"projects"`
	}
	must(t, json.Unmarshal(w.Body.Bytes(), &resp))
	got := map[string]map[string]any{}
	for _, p := range resp.Projects {
		got[p["name"].(string)] = p
	}
	check := func(name string, ops []any, operable, managed bool, blocked any) {
		t.Helper()
		p := got[name]
		if p == nil {
			t.Fatalf("%s missing from %s", name, w.Body.String())
		}
		if !reflect.DeepEqual(p["allowed_ops"], ops) || p["operable"] != operable || p["managed"] != managed || p["ops_blocked"] != blocked {
			t.Errorf("%s: got %v", name, p)
		}
		if p["working_dir"] == nil {
			t.Errorf("%s: the durable entry fields must still be there: %v", name, p)
		}
	}
	check("docker-agent", []any{}, false, false, "self")
	check("gdrive-agent", []any{"down", "restart"}, false, false, "outside_compose_root")
	check("traefik", []any{"pull", "recreate", "restart", "up", "update"}, true, true, "control_path")
}

// TestFleetSnapshotStampsSelf runs the production snapshot build against the fake
// engine: the self row, the container flags, and the registry working dir.
func TestFleetSnapshotStampsSelf(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "gdrive-agent", WorkingDir: "/srv/containers/gdrive-agent-registered"}))
	hub := newFleetHub(e.a)
	snap, err := hub.hub.SnapshotNow(context.Background())
	must(t, err)
	raw, err := json.Marshal(snap)
	must(t, err)

	byProject := map[string]ComposeProject{}
	for _, p := range snap.Data.ComposeProjects {
		byProject[p.Name] = p
	}
	if p := byProject["docker-agent"]; p.OpsBlocked != blockedSelf || p.AllowedOps == nil || len(p.AllowedOps) != 0 {
		t.Fatalf("self row: %+v", p)
	}
	if p := byProject["traefik"]; p.OpsBlocked != blockedControlPath {
		t.Fatalf("control-path row: %+v", p)
	}
	if p := byProject["gdrive-agent"]; p.WorkingDir != "/srv/containers/gdrive-agent-registered" {
		t.Fatalf("registered row must report the registry dir, got %q", p.WorkingDir)
	}
	byName := map[string]ContainerStatus{}
	for _, c := range snap.Data.Containers {
		byName[c.Name] = c
	}
	if !byName["docker-agent"].Self || byName["traefik"].Self || !byName["traefik"].ControlPath || byName["gdrive-agent"].Self {
		t.Fatalf("container flags: %+v", byName)
	}
	for _, want := range []string{`"allowed_ops":[]`, `"self":true`, `"control_path":true`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("snapshot wire missing %s", want)
		}
	}
	if st := e.a.self.status(); st["name"] != "docker-agent" {
		t.Errorf("the snapshot build must publish self identity; readiness shows %v", st)
	}
}

// TestReadinessNeverWaitsOnTheDaemon: readiness reads the published view; a hung
// container list must not hold it.
func TestReadinessNeverWaitsOnTheDaemon(t *testing.T) {
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
		return append(defaultContainers(root),
			fakeContainer{id: strings.Repeat("5", 64), name: "netshare", networkMode: "container:" + testSelfID})
	})
	hang := make(chan struct{})
	e.eng.set(func(f *fakeEngine) { f.listHang = hang })

	// A handler blocked on the hung list, concurrently.
	go func() { _, _ = e.do(t, http.MethodPost, "/v1/containers/gdrive-agent/stop", nil) }()
	time.Sleep(50 * time.Millisecond)

	done := make(chan map[string]any, 1)
	go func() {
		_, body := e.do(t, http.MethodGet, "/health/ready", nil)
		done <- body
	}()
	select {
	case body := <-done:
		self, _ := body["self"].(map[string]any)
		if body["status"] != "ready" || self["container_id"] != testSelfID[:12] || !strings.Contains(self["error"].(string), "not yet observed") {
			t.Fatalf("got %v", body)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness blocked behind a hung container list")
	}
	close(hang)

	// After a successful list, readiness shows what is protected.
	_, _, err := e.a.observeNow(context.Background())
	must(t, err)
	_, body := e.do(t, http.MethodGet, "/health/ready", nil)
	self, _ := body["self"].(map[string]any)
	cp, _ := self["control_path"].(map[string]any)
	if self["name"] != "docker-agent" || cp["project"] != "traefik" || !reflect.DeepEqual(self["ambiguous"], []any{"netshare"}) {
		t.Fatalf("readiness self: %v", self)
	}
}

func TestReadinessReportsMountinfoFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := newSelfIdentity(filepath.Join(t.TempDir(), "absent"), "traefik")
	a := &app{self: s}
	r := gin.New()
	registerRoutes(r, a)
	status, body := doJSON(t, r, http.MethodGet, "/health/ready", nil)
	if status != http.StatusOK || body["status"] != "ready" {
		t.Fatalf("readiness must stay 200/ready, got %d %v", status, body)
	}
	self, _ := body["self"].(map[string]any)
	if self == nil || !strings.Contains(self["error"].(string), "absent") {
		t.Fatalf("readiness must surface the self-identity failure, got %v", body)
	}
}

// TestNewAppWiresSelfIdentity runs the production constructor: the mountinfo
// read, the control-path name from config, and a snapshot that stamps self.
func TestNewAppWiresSelfIdentity(t *testing.T) {
	root := t.TempDir()
	eng := newFakeEngine(t, defaultContainers(root)...)
	mi := filepath.Join(t.TempDir(), "mountinfo")
	must(t, os.WriteFile(mi, []byte(strings.Join([]string{
		mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/hostname", "/etc/hostname"),
		mountinfoLine("/var/lib/docker/containers/"+testSelfID+"/hosts", "/etc/hosts"),
	}, "\n")), 0o644))
	prev := mountinfoPath
	mountinfoPath = mi
	t.Cleanup(func() { mountinfoPath = prev })

	cfg := Config{
		NodeName: "node01", SiteID: "site-a", ConfigDir: t.TempDir(), DockerHost: eng.host(),
		ComposeRoot: root, ComposeRegistryPath: filepath.Join(root, "projects.json"),
		ControlCenterURL: "https://controller.example.invalid:1443", TraefikDockerDNS: "traefik", TraefikDialPort: "1443",
	}
	a, err := newApp(context.Background(), cfg)
	must(t, err)
	t.Cleanup(a.close)
	if a.self.containerID != testSelfID || a.self.controlPathName != "traefik" {
		t.Fatalf("self identity not wired: id=%q controlPath=%q err=%v", a.self.containerID, a.self.controlPathName, a.self.idErr)
	}
	snap, err := a.fleet.hub.SnapshotNow(context.Background())
	must(t, err)
	for _, p := range snap.Data.ComposeProjects {
		if p.Name == "docker-agent" && p.OpsBlocked != blockedSelf {
			t.Fatalf("app-built snapshot does not protect the agent: %+v", p)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
