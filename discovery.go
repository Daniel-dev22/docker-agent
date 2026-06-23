package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// Phase 4 — discovery feed.
//
// discoveryPusher periodically POSTs the live fleet snapshot to controller's
// POST /api/docker/discovery, which materializes it into the shared `discovery`
// table — the same source the action dropdowns + browse pages read. This is the
// agent-native REPLACEMENT for the portainer-scan → MQTT → mqtt-ingest producer
// for docker hosts. The agent has no MQTT client by design (adding one would
// re-introduce the scan→MQTT→ingest chain we're retiring), so the existing
// durable, per-node-authenticated mTLS HTTP path is reused.
//
// Discovery is a FULL-SNAPSHOT, latest-wins feed: the router DELETEs + re-inserts
// every record for this (site,node,platform,domain) on each push. So it needs no
// durable outbox (unlike job events, which are append-only and each one matters).
// The feed is a derived shadow of live docker state (the engine + the image-check
// cache) — the source of truth lives elsewhere. State-persistence litmus test:
// "if the agent restarts right now, the next cycle will…" → re-push the complete
// snapshot within DOCKER_DISCOVERY_INTERVAL. No disk, no replay, correct on restart.
//
// Cadence: a periodic tick (default 5m) PLUS an out-of-band trigger fired after
// every image-check pass, so a freshly-detected outdated image surfaces in the
// dropdowns within seconds rather than waiting for the next tick.
// ---------------------------------------------------------------------------

// DiscoveryPayload is the wire body of POST /api/docker/discovery. The router
// fans it out into discovery records under platform='docker' (containers +
// compose_projects) and, during migration, the legacy platform='portainer',
// domain='stacks' shape so the existing portainer-stack-update dropdown resolves.
type DiscoveryPayload struct {
	Site            string            `json:"site"`
	Node            string            `json:"node"`
	Containers      []ContainerStatus `json:"containers"`
	ComposeProjects []ComposeProject  `json:"compose_projects"`
	// Networks (platform=docker, domain=networks) — agent-native replacement for
	// the retired ansible docker_network_metrics collector. POINTER: nil means
	// "network list failed this push, don't touch existing rows"; a non-nil
	// (even empty) slice is authoritative and replaces them. Containers/compose
	// are always authoritative so they stay plain slices.
	Networks *[]NetworkStatus `json:"networks,omitempty"`
	// TraefikNetworks (platform=traefik, domain=networks) — the traefik
	// container's network attachments, for entrypoint management. Always
	// authoritative (empty when no traefik container on this host).
	TraefikNetworks []ContainerNetAttachment `json:"traefik_networks"`
	EmittedAt       time.Time                `json:"emitted_at"`
}

type discoveryPusher struct {
	cfg      Config
	cc       *http.Client
	fleet    *fleetHub
	docker   *dockerClient
	interval time.Duration
	trigger  chan struct{}

	firstDone  bool
	lastPushOK bool

	// Network gather TTL cache (single-goroutine: only touched from Run's select
	// loop, so no lock). Bounds docker-socket cost to ~2 calls per netTTL window
	// rather than per push — keeps a low-power Pi cheap when a periodic tick and a
	// post-image-check trigger land close together.
	netTTL          time.Duration
	netCachedAt     time.Time
	netCacheOK      bool
	netCache        []NetworkStatus
	netTraefikCache []ContainerNetAttachment
}

func newDiscoveryPusher(a *app) *discoveryPusher {
	return &discoveryPusher{
		cfg:      a.cfg,
		cc:       a.cc,
		fleet:    a.fleet,
		docker:   a.docker,
		interval: getEnvDuration("DOCKER_DISCOVERY_INTERVAL", 5*time.Minute),
		netTTL:   getEnvDuration("DOCKER_NETWORK_TTL", 120*time.Second),
		trigger:  make(chan struct{}, 1),
	}
}

// gatherNetworks returns the docker network list (platform=docker/networks) and
// the traefik container's attachments (platform=traefik/networks), TTL-cached.
//
// Gathering is intentionally CHEAP and FIXED-COST regardless of network count:
// ONE batch NetworkList call (the summary already carries IPAM/options — no
// per-network inspect/fan-out) plus ONE ContainerInspect for traefik = 2 socket
// calls. There is nothing to parallelize, so unlike the image checker (which
// fans out one registry call per image and caps concurrency via GOMAXPROCS),
// this needs no concurrency throttle. The TTL just avoids re-running those 2
// calls when the periodic push and the post-image-check trigger fire close
// together. On a list error we don't cache (retry next push) and omit networks
// so the router keeps the last-good rows instead of wiping them.
func (d *discoveryPusher) gatherNetworks(ctx context.Context) (*[]NetworkStatus, []ContainerNetAttachment) {
	if d.netCacheOK && time.Since(d.netCachedAt) < d.netTTL {
		out := d.netCache
		return &out, d.netTraefikCache
	}
	nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	nets, err := d.docker.listNetworks(nctx)
	traefik := d.docker.containerNetAttachments(nctx, "traefik")
	cancel()
	if err != nil {
		slog.Warn("docker network list failed; pushing without networks", "node", d.cfg.NodeName, "error", err)
		return nil, traefik
	}
	d.netCache = nets
	d.netTraefikCache = traefik
	d.netCachedAt = time.Now()
	d.netCacheOK = true
	out := nets
	return &out, traefik
}

// Trigger requests an out-of-band push (coalesced). Called by the image checker
// after each pass so newly-detected image status reaches discovery promptly.
func (d *discoveryPusher) Trigger() {
	if d == nil {
		return
	}
	select {
	case d.trigger <- struct{}{}:
	default:
	}
}

// Run drives the periodic push + on-demand triggers. A short jittered first fire
// lands initial data quickly without a fleet-wide thundering herd on the router.
func (d *discoveryPusher) Run(ctx context.Context) {
	first := 12*time.Second + jitterFor(d.cfg.NodeName, 18*time.Second)
	timer := time.NewTimer(first)
	defer timer.Stop()
	slog.Info("discovery feed started", "interval", d.interval, "first_in", first)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			d.push(ctx)
			timer.Reset(d.interval + jitterFor(d.cfg.NodeName, 30*time.Second))
		case <-d.trigger:
			d.push(ctx)
		}
	}
}

func (d *discoveryPusher) push(ctx context.Context) {
	bctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	snap, err := d.fleet.hub.SnapshotNow(bctx)
	cancel()
	if err != nil {
		d.report(false, "snapshot failed: "+err.Error())
		return
	}

	// Networks are gathered here (not in the high-frequency fleet WS snapshot —
	// they change rarely), TTL-cached so frequent pushes don't re-hit the socket.
	netsPtr, traefikNets := d.gatherNetworks(ctx)

	payload := DiscoveryPayload{
		Site:            d.cfg.SiteID,
		Node:            d.cfg.NodeName,
		Containers:      snap.Data.Containers,
		ComposeProjects: snap.Data.ComposeProjects,
		Networks:        netsPtr,
		TraefikNetworks: traefikNets,
		EmittedAt:       time.Now().UTC(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		d.report(false, "marshal failed: "+err.Error())
		return
	}

	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodPost,
		d.cfg.ControlCenterURL+"/api/docker/discovery", bytes.NewReader(body))
	if err != nil {
		d.report(false, "build request failed: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.cc.Do(req)
	if err != nil {
		d.report(false, "post failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	// Drain so the keep-alive connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		d.report(false, "router status "+resp.Status)
		return
	}
	d.report(true, "")
}

// report logs on the first push and on every health transition only (the
// logging-in-loops rule: never log every cycle; confirm-on-first + on-change),
// carrying the container/project counts so a healthy feed is observable once.
func (d *discoveryPusher) report(ok bool, detail string) {
	transition := !d.firstDone || ok != d.lastPushOK
	d.firstDone = true
	d.lastPushOK = ok
	if !transition {
		return
	}
	if ok {
		slog.Info("discovery feed push ok", "node", d.cfg.NodeName, "site", d.cfg.SiteID)
	} else {
		slog.Warn("discovery feed push failing", "node", d.cfg.NodeName, "detail", detail)
	}
}
