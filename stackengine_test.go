package main

import (
	"context"
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
