package main

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// The scenario the design centers on: a coupled (upstream-compose) project whose
// DRIVER is current but a coupled service (redis) has its OWN newer upstream.
// Per-service detection would flag redis outdated; plan-driven detection must
// report the WHOLE STACK updated, because the driver release would change nothing.
func TestStampPlanWinsOverPerServiceRollup(t *testing.T) {
	containers := []ContainerStatus{
		{Name: "immich-server-1", ComposeProject: "immich", ComposeService: "immich-server", Image: "ghcr.io/immich-app/immich-server:v2.7.5"},
		{Name: "redis-1", ComposeProject: "immich", ComposeService: "redis", Image: "docker.io/valkey/valkey:8"},
	}
	// Per-service cache (what an independent check found): redis is "outdated"
	// against its own upstream (valkey 9 exists), server is "updated".
	ic := &imageChecker{
		cache: map[string]imageCheck{
			compositeKey(containers[0]): {ImageStatus: statusUpdated},
			compositeKey(containers[1]): {ImageStatus: statusOutdated, LatestImageVersion: "docker.io/valkey/valkey:9"},
		},
		// The resolver dry-run: the driver release (immich) would change NOTHING.
		projPlans: map[string]projPlan{
			"immich": {kind: "upstream-compose", targets: map[string]string{}},
		},
	}
	projects := []ComposeProject{{Name: "immich"}}
	stampImageStatus(containers, projects, ic)

	if projects[0].ImageStatus != statusUpdated {
		t.Errorf("project = %q, want updated (driver unchanged ⇒ whole stack updated)", projects[0].ImageStatus)
	}
	if projects[0].OutdatedCount != 0 {
		t.Errorf("outdated_count = %d, want 0", projects[0].OutdatedCount)
	}
	for _, c := range containers {
		if c.ImageStatus != statusUpdated {
			t.Errorf("container %s = %q, want updated (plan wins over its independent check)", c.ComposeService, c.ImageStatus)
		}
	}
}

// When the driver release DOES move a service, that service (and the project)
// are outdated; unchanged coupled services stay updated.
func TestStampPlanDriverMoved(t *testing.T) {
	containers := []ContainerStatus{
		{ComposeProject: "immich", ComposeService: "immich-server", Image: "ghcr.io/immich-app/immich-server:v2.7.0"},
		{ComposeProject: "immich", ComposeService: "redis", Image: "docker.io/valkey/valkey:8"},
	}
	ic := &imageChecker{
		cache: map[string]imageCheck{},
		projPlans: map[string]projPlan{
			"immich": {kind: "upstream-compose", targets: map[string]string{
				"immich-server": "ghcr.io/immich-app/immich-server:v2.7.5",
			}},
		},
	}
	projects := []ComposeProject{{Name: "immich"}}
	stampImageStatus(containers, projects, ic)

	if projects[0].ImageStatus != statusOutdated || projects[0].OutdatedCount != 1 {
		t.Errorf("project = %q count=%d, want outdated/1", projects[0].ImageStatus, projects[0].OutdatedCount)
	}
	got := map[string]string{}
	for _, c := range containers {
		got[c.ComposeService] = c.ImageStatus
	}
	if got["immich-server"] != statusOutdated {
		t.Errorf("immich-server = %q, want outdated", got["immich-server"])
	}
	if got["redis"] != statusUpdated {
		t.Errorf("redis = %q, want updated (not in the driver plan)", got["redis"])
	}
}

// overrideResolver.plan returns an empty change set when already on the pinned
// image — so detection reports updated and the engine skips a force-recreate.
func TestOverrideResolverPlanNoChangeWhenCurrent(t *testing.T) {
	e := &engine{}
	proj := &types.Project{Name: "p", Services: types.Services{"svc": types.ServiceConfig{Name: "svc", Image: "img:1.0"}}}
	got, err := (&overrideResolver{e: e, image: "img:1.0", service: "svc"}).plan(nil, proj, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("plan = %v, want empty (already on pinned image)", got)
	}
	// Different pinned image → a change.
	got, _ = (&overrideResolver{e: e, image: "img:2.0", service: "svc"}).plan(nil, proj, func(string) {})
	if got["svc"] != "img:2.0" {
		t.Errorf("plan = %v, want {svc: img:2.0}", got)
	}
}
