package main

// Network reconcile — the last step of a stack update. After an update recreates
// a stack, a container can occasionally come back attached to FEWER networks than
// its compose service declares (most often when a shared/external network was
// itself recreated during the deploy). This detects that drift and issues ONE
// corrective `up --force-recreate`.
//
// Best-effort: it NEVER fails the update (the deploy already health-passed). It
// compares the count of networks the compose service declares against the count
// the running container is actually attached to — a cheap, name-resolution-free
// signal that's robust to compose's project-scoped vs external network naming.

import (
	"context"
	"fmt"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
)

// reconcileNetworks checks each project container for missing network
// attachments and, if any are short, recreates the stack once. Best-effort.
func (e *engine) reconcileNetworks(ctx context.Context, j *Job, entry ProjectEntry, project *types.Project, fleetTrigger func()) {
	// Expected network count per service (host network_mode services declare none).
	want := map[string]int{}
	for name, svc := range project.Services {
		if svc.NetworkMode != "" {
			continue // host/none/container: not on bridge networks
		}
		want[name] = len(svc.Networks)
	}
	if len(want) == 0 {
		return
	}

	base := e.snapshotProject(ctx, entry.Name)
	detached := false
	for _, c := range base.containers {
		expected, ok := want[c.service]
		if !ok || expected == 0 {
			continue
		}
		nctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		nets, err := e.docker.containerNetworks(nctx, c.id)
		cancel()
		if err != nil {
			continue
		}
		if len(nets) < expected {
			j.appendLine(fmt.Sprintf("network drift: %s on %d/%d networks", c.name, len(nets), expected))
			detached = true
		}
	}
	if !detached {
		return
	}

	j.appendLine("reconciling networks (up --force-recreate)")
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := e.compose.upProject(rctx, j, project, api.RecreateForce, fleetTrigger); err != nil {
		j.appendLine("network reconcile failed (non-fatal): " + err.Error())
		return
	}
	fleetTrigger()
}
