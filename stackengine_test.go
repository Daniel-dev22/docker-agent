package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestResolverKind(t *testing.T) {
	tests := []struct {
		name string
		o    strategyOverride
		want string
	}{
		{"explicit wins", strategyOverride{ImageResolver: "build-agent", ServiceMap: map[string]string{"a": "b"}}, "build-agent"},
		{"service_map ⇒ upstream-compose", strategyOverride{ServiceMap: map[string]string{"a": "b"}}, "upstream-compose"},
		{"upstream_compose ⇒ upstream-compose", strategyOverride{UpstreamCompose: "http://x/{tag}"}, "upstream-compose"},
		{"version_source override ⇒ override", strategyOverride{VersionSource: "override"}, "override"},
		{"explicit image ⇒ override", strategyOverride{Image: "repo:tag"}, "override"},
		{"default ⇒ registry", strategyOverride{VersionSource: "github-release"}, "registry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolverKind(tc.o); got != tc.want {
				t.Fatalf("resolverKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProjectBaselineHelpers(t *testing.T) {
	b := projectBaseline{containers: []containerBaseline{
		{name: "p-web-1", id: "id1", service: "web", imageID: "sha256:aaa"},
		{name: "p-web-2", id: "id2", service: "web", imageID: "sha256:aaa"},
		{name: "p-db-1", id: "id3", service: "db", imageID: "sha256:bbb"},
	}}

	names := b.containerNames()
	if len(names) != 3 || names[0] != "p-db-1" {
		t.Fatalf("containerNames sorted = %v", names)
	}
	prev := b.previousIDs()
	if prev["p-web-1"] != "id1" || prev["p-db-1"] != "id3" {
		t.Fatalf("previousIDs = %v", prev)
	}
	sid := b.serviceImageIDs()
	if sid["web"] != "sha256:aaa" || sid["db"] != "sha256:bbb" || len(sid) != 2 {
		t.Fatalf("serviceImageIDs = %v", sid)
	}
	scope := b.servicesForContainers([]string{"p-db-1"})
	if len(scope) != 1 || scope[0] != "db" {
		t.Fatalf("servicesForContainers = %v", scope)
	}
	all := b.allServices()
	if len(all) != 2 {
		t.Fatalf("allServices = %v", all)
	}
}

func TestApplyImages(t *testing.T) {
	project := &types.Project{Services: types.Services{
		"web": types.ServiceConfig{Name: "web", Image: "repo/web:1"},
		"db":  types.ServiceConfig{Name: "db", Image: "postgres:16"},
	}}
	applyImages(project, map[string]string{"web": "repo/web:2", "missing": "x:1"})
	if project.Services["web"].Image != "repo/web:2" {
		t.Fatalf("web image = %q", project.Services["web"].Image)
	}
	if project.Services["db"].Image != "postgres:16" {
		t.Fatalf("db image changed unexpectedly = %q", project.Services["db"].Image)
	}
}

func TestHealthTimeoutsPerHost(t *testing.T) {
	t.Setenv("DOCKER_HEALTH_SWAP_TIMEOUT", "")
	t.Setenv("DOCKER_HEALTH_TIMEOUT", "")
	pi := &engine{cfg: Config{NodeName: "pi"}}
	s, h := pi.healthTimeouts()
	if s.Seconds() != 300 || h.Seconds() != 360 {
		t.Fatalf("pi timeouts = %v / %v", s, h)
	}
	nuc := &engine{cfg: Config{NodeName: "nuc"}}
	s, h = nuc.healthTimeouts()
	if s.Seconds() != 120 || h.Seconds() != 180 {
		t.Fatalf("nuc timeouts = %v / %v", s, h)
	}
}

func TestNotHealthy(t *testing.T) {
	hm := map[string]string{
		"a": "healthy", "b": "unhealthy", "c": "none",
		"d": "crash_loop", "e": "excluded", "f": "not_running",
	}
	excl := map[string]bool{"e": true}
	got := notHealthy(hm, excl)
	want := map[string]bool{"b": true, "d": true, "f": true}
	if len(got) != len(want) {
		t.Fatalf("notHealthy = %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected unhealthy %q in %v", n, got)
		}
	}
}

func TestOverrideResolverServiceInference(t *testing.T) {
	e := &engine{}
	// Project named like a service → that service.
	proj := &types.Project{Name: "traefik", Services: types.Services{
		"traefik": types.ServiceConfig{Name: "traefik", Image: "traefik:v3"},
		"sidecar": types.ServiceConfig{Name: "sidecar", Image: "x:1"},
	}}
	r := &overrideResolver{e: e, image: "traefik:v3.1"}
	got, err := r.plan(context.Background(), proj, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if got["traefik"] != "traefik:v3.1" || len(got) != 1 {
		t.Fatalf("override (project-named) = %v", got)
	}

	// Single-service project → the sole service.
	single := &types.Project{Name: "x", Services: types.Services{"only": types.ServiceConfig{Name: "only", Image: "a:1"}}}
	got, err = (&overrideResolver{e: e, image: "a:2"}).plan(context.Background(), single, func(string) {})
	if err != nil || got["only"] != "a:2" {
		t.Fatalf("override (single) = %v err=%v", got, err)
	}

	// Ambiguous multi-service, no hint → error.
	ambig := &types.Project{Name: "stack", Services: types.Services{
		"a": types.ServiceConfig{Name: "a", Image: "a:1"},
		"b": types.ServiceConfig{Name: "b", Image: "b:1"},
	}}
	if _, err := (&overrideResolver{e: e, image: "z:1"}).plan(context.Background(), ambig, func(string) {}); err == nil {
		t.Fatal("expected error for ambiguous override target")
	}
}

func TestEnvVarRefExtraction(t *testing.T) {
	cases := map[string]string{
		"${TRAEFIK_IMAGE}":      "TRAEFIK_IMAGE",
		"${VAR:-default:tag}":   "VAR",
		"$PLAIN":                "PLAIN",
		"ghcr.io/foo/bar:1.2.3": "",
		"repo:tag":              "",
	}
	for in, want := range cases {
		m := envVarRefRe.FindStringSubmatch(in)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != want {
			t.Fatalf("envVarRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetServiceImageLiteralAndVar(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	env := filepath.Join(dir, ".env")
	composeBody := "services:\n" +
		"  traefik:\n" +
		"    image: traefik:v3.0\n" +
		"  immich:\n" +
		"    image: ${IMMICH_IMAGE}\n"
	if err := os.WriteFile(compose, []byte(composeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("IMMICH_IMAGE=ghcr.io/immich-app/immich:v1.0.0\nOTHER=keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := ProjectEntry{Name: "p", WorkingDir: dir, ComposeFiles: []string{"docker-compose.yml"}, EnvFiles: []string{".env"}}

	// Literal rewrite.
	old, err := setServiceImage(entry, "traefik", "traefik:v3.1")
	if err != nil || old.literal != "traefik:v3.0" {
		t.Fatalf("literal old=%+v err=%v", old, err)
	}
	data, _ := os.ReadFile(compose)
	if !contains(string(data), "traefik:v3.1") {
		t.Fatalf("compose not rewritten: %s", data)
	}

	// Var rewrite goes to .env, leaves the ${VAR} scalar intact.
	old, err = setServiceImage(entry, "immich", "ghcr.io/immich-app/immich:v2.0.0")
	if err != nil || old.env == nil || old.env.line != "IMMICH_IMAGE=ghcr.io/immich-app/immich:v1.0.0" {
		t.Fatalf("var old=%+v err=%v", old, err)
	}
	edata, _ := os.ReadFile(env)
	if !contains(string(edata), "IMMICH_IMAGE=ghcr.io/immich-app/immich:v2.0.0") || !contains(string(edata), "OTHER=keep") {
		t.Fatalf(".env not updated correctly: %s", edata)
	}
	cdata, _ := os.ReadFile(compose)
	if !contains(string(cdata), "${IMMICH_IMAGE}") {
		t.Fatalf("var scalar should be preserved: %s", cdata)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
