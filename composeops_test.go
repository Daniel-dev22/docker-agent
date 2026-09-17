package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
)

func opsProject() *types.Project {
	return &types.Project{Name: "stack", Services: types.Services{
		"db":  {Name: "db", Image: "postgres:16"},
		"app": {Name: "app", Image: "reg/app:v1", DependsOn: types.DependsOnConfig{"db": {Condition: "service_started", Required: true}}},
		"web": {Name: "web", Image: "nginx:alpine"},
	}}
}

// A service-narrowed op acts on exactly the named services: no dependency is
// started, no orphan removed, and down never reaches networks or volumes.
func TestPlanComposeCallNarrowsToTheNamedServices(t *testing.T) {
	grace := 7 * time.Second
	for _, op := range []string{opComposeUp, opComposeRecreate} {
		call, err := planComposeCall(op, "stack", opsProject(), []string{"app"}, nil)
		must(t, err)
		if call.kind != callUp || !slices.Equal(call.project.ServiceNames(), []string{"app"}) {
			t.Fatalf("%s: kind %v, services %v — the stopped dependency db must not be in the project", op, call.kind, call.project.ServiceNames())
		}
		if !slices.Equal(call.up.Create.Services, []string{"app"}) || !slices.Equal(call.up.Start.Services, []string{"app"}) ||
			call.up.Create.RemoveOrphans || call.up.Start.Project != call.project {
			t.Fatalf("%s: %+v", op, call.up)
		}
		if (op == opComposeRecreate) != (call.up.Create.Recreate == api.RecreateForce) {
			t.Fatalf("%s: recreate strategy %q", op, call.up.Create.Recreate)
		}
	}

	call, err := planComposeCall(opComposePull, "stack", opsProject(), []string{"app", "web"}, nil)
	must(t, err)
	if call.kind != callPull || !slices.Equal(slices.Sorted(slices.Values(call.project.ServiceNames())), []string{"app", "web"}) {
		t.Fatalf("pull: %v %v", call.kind, call.project.ServiceNames())
	}

	call, err = planComposeCall(opComposeRestart, "stack", opsProject(), []string{"app"}, &grace)
	must(t, err)
	if call.kind != callRestart || !call.restart.NoDeps || !slices.Equal(call.restart.Services, []string{"app"}) || call.restart.Timeout != &grace {
		t.Fatalf("restart: %+v", call.restart)
	}

	call, err = planComposeCall(opComposeDown, "stack", opsProject(), []string{"app"}, nil)
	must(t, err)
	if call.kind != callRemove || !call.remove.Stop || !call.remove.Force || call.remove.Volumes || !slices.Equal(call.remove.Services, []string{"app"}) {
		t.Fatalf("down [app] must be stop + remove of app's containers only, keeping volumes: %+v", call.remove)
	}

	// Whole-project calls are what they were.
	if call, _ := planComposeCall(opComposeDown, "stack", nil, nil, nil); call.kind != callDown || !call.down.RemoveOrphans || call.down.Volumes {
		t.Fatalf("whole down: %+v", call)
	}
	if call, _ := planComposeCall(opComposeUp, "stack", opsProject(), nil, nil); call.kind != callUp || !call.up.Create.RemoveOrphans || len(call.project.Services) != 3 {
		t.Fatalf("whole up: %+v", call)
	}
	if _, err := planComposeCall(opComposeUp, "stack", opsProject(), []string{"nope"}, nil); err == nil {
		t.Fatal("an unknown service must not plan")
	}
}

func TestComposeOpServicesAreValidated(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	cb, err := newComposeBackend(Config{DockerHost: e.eng.host(), ComposeRoot: e.root})
	must(t, err)
	e.a.compose = cb
	compose := "services:\n  app:\n    image: nginx:alpine\n  web:\n    image: nginx:alpine\n"
	if status, body := e.do(t, http.MethodPost, "/v1/projects", map[string]any{"name": "svcproj", "files": map[string]string{"compose.yaml": compose}}); status != http.StatusOK {
		t.Fatalf("fixture register: %d %v", status, body)
	}

	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"unknown service", map[string]any{"op": "up", "services": []string{"app", "zzz"}}, 400, "unknown_service"},
		{"not a compose service name", map[string]any{"op": "down", "services": []string{"bad name!"}}, 400, "unknown_service"},
		{"services on update", map[string]any{"op": "update", "services": []string{"app"}}, 400, "invalid_body"},
	}
	for _, c := range cases {
		status, body := e.do(t, http.MethodPost, "/v1/projects/svcproj/op", c.body)
		if status != c.status || body["code"] != c.code {
			t.Errorf("%s: %d %v", c.name, status, body)
		}
	}
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'a'
	}
	if status, body := e.do(t, http.MethodPost, "/v1/projects/svcproj/op", map[string]any{"op": "up", "services": []string{string(long)}}); status != 400 || len(body["service"].(string)) >= 600 {
		t.Errorf("a long unknown name is echoed clipped: %d %v", status, body)
	}

	status, body := e.do(t, http.MethodPost, "/v1/projects/svcproj/op", map[string]any{"op": "up", "services": []string{"web", "app", "web"}})
	if status != http.StatusAccepted {
		t.Fatalf("valid services: %d %v", status, body)
	}
	j, ok := e.a.reg.get(body["job_id"].(string))
	if !ok || !slices.Equal(j.services, []string{"app", "web"}) {
		t.Fatalf("the job carries services %v, want sorted and deduplicated", j.services)
	}
}

// The Idempotency fingerprint treats services as a set.
func TestFingerprintTreatsServicesAsASet(t *testing.T) {
	a := bodyFingerprint([]byte(`{"op":"up","services":["web","app","app"]}`))
	b := bodyFingerprint([]byte(`{"services":["app","web"],"op":"up"}`))
	c := bodyFingerprint([]byte(`{"op":"up","services":["app"]}`))
	d := bodyFingerprint([]byte(`{"op":"up"}`))
	if a != b {
		t.Error("the same set in another order or with a duplicate fingerprints differently")
	}
	if a == c || c == d {
		t.Error("different service sets must fingerprint differently")
	}
}

// Clients detect the feature from GET /v1/projects.
func TestProjectsAdvertiseServiceOps(t *testing.T) {
	e := newCapEnv(t, testSelfID, defaultContainers)
	must(t, e.a.projects.register(ProjectEntry{Name: "adv", WorkingDir: e.root + "/adv"}))
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	var resp struct {
		Projects []map[string]any `json:"projects"`
	}
	must(t, json.Unmarshal(w.Body.Bytes(), &resp))
	if len(resp.Projects) == 0 || resp.Projects[0]["service_ops"] != true {
		t.Fatalf("service_ops not advertised: %s", w.Body.String())
	}
}

// A project outside the compose root narrows down and restart from its containers'
// labels, as it runs them whole — no file is read, and nothing answers an uncoded
// 5xx for a deterministic condition.
func TestNarrowedOpsWorkFromLabels(t *testing.T) {
	e := newCapEnv(t, testSelfID, func(root string) []fakeContainer {
		return append(defaultContainers(root),
			fakeContainer{id: "cafe" + strings.Repeat("0", 60), name: "dup-web-1", project: "duplicacy", workingDir: "/srv/duplicacy", service: "web"},
			fakeContainer{id: "cafe" + strings.Repeat("1", 60), name: "dup-cli-1", project: "duplicacy", workingDir: "/srv/duplicacy", service: "cli"},
		)
	})
	cb, err := newComposeBackend(Config{DockerHost: e.eng.host(), ComposeRoot: e.root})
	must(t, err)
	e.a.compose = cb
	must(t, e.a.projects.register(ProjectEntry{Name: "duplicacy", WorkingDir: "/srv/duplicacy"}))
	must(t, os.MkdirAll(filepath.Join(e.root, "broken"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.root, "broken", "compose.yaml"), []byte("services: [not, a, map]\n"), 0o644))
	must(t, e.a.projects.register(ProjectEntry{Name: "broken", WorkingDir: filepath.Join(e.root, "broken")}))

	cases := []struct {
		name, project string
		body          map[string]any
		status        int
		code          string
	}{
		{"narrowed restart outside the root", "duplicacy", map[string]any{"op": "restart", "services": []string{"web"}}, 202, ""},
		{"narrowed down outside the root", "duplicacy", map[string]any{"op": "down", "services": []string{"cli", "web"}}, 202, ""},
		{"a service with no containers", "duplicacy", map[string]any{"op": "restart", "services": []string{"nope"}}, 400, "unknown_service"},
		{"another project's service", "duplicacy", map[string]any{"op": "down", "services": []string{"gdrive-agent"}}, 400, "unknown_service"},
		{"up outside the root is refused by capability first", "duplicacy", map[string]any{"op": "up", "services": []string{"web"}}, 409, "project_not_operable"},
		{"narrowed up on a project whose files do not load", "broken", map[string]any{"op": "up", "services": []string{"web"}}, 409, "project_load_failed"},
	}
	for _, c := range cases {
		status, body := e.do(t, http.MethodPost, "/v1/projects/"+c.project+"/op", c.body)
		if status != c.status || (c.code != "" && body["code"] != c.code) {
			t.Errorf("%s: %d %v", c.name, status, body)
		}
		if status >= 500 {
			t.Errorf("%s: a deterministic condition answered %d without a code: %v", c.name, status, body)
		}
		if status == http.StatusAccepted {
			j, _ := e.a.reg.get(body["job_id"].(string))
			snap := j.snapshot()
			want := c.body["services"].([]string)
			if !slices.Equal(snap.Services, want) || snap.Target != strings.Join(want, ",") {
				t.Errorf("%s: the job record reads %v / target %q — a narrowed op must be recorded as narrowed", c.name, snap.Services, snap.Target)
			}
			raw, _ := json.Marshal(snap)
			if !strings.Contains(string(raw), `"services":[`) {
				t.Errorf("%s: the public job JSON omits services: %s", c.name, raw)
			}
		}
	}

	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/projects", nil))
	var resp struct {
		Projects []map[string]any `json:"projects"`
	}
	must(t, json.Unmarshal(w.Body.Bytes(), &resp))
	for _, p := range resp.Projects {
		ops, _ := p["allowed_ops"].([]any)
		narrowable := slices.ContainsFunc(ops, func(o any) bool { return o != "update" })
		if p["service_ops"] != narrowable {
			t.Errorf("%s: service_ops=%v with allowed_ops %v — the advertisement must match what the ops honour", p["name"], p["service_ops"], ops)
		}
	}
}

// The event the controller stores carries the services, and docker_jobs.target
// holds them joined, so its history does not read whole-stack.
func TestNarrowedOpEventCarriesServices(t *testing.T) {
	j := newJob("j1", JobRequest{Operation: opComposeDown, Project: "p", Services: []string{"a", "b"}, Target: "a,b"}, nil)
	var got EventPayload
	payload, err := (&eventBuffer{}).payloadFor(j.snapshot(), EventStarted)
	must(t, err)
	must(t, json.Unmarshal(payload, &got))
	if got.Target != "a,b" || !slices.Equal(got.Services, []string{"a", "b"}) {
		t.Fatalf("event payload: %s", payload)
	}
}
