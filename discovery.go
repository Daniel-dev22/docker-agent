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
// Discovery feed.
//
// discoveryPusher periodically POSTs the live fleet snapshot to the controller's
// POST /api/docker/discovery, which materializes it into whatever inventory the
// controller serves to its UI. The agent speaks only HTTP over the same durable,
// per-node-authenticated path everything else uses — it has no message-bus client
// by design.
//
// Discovery is a FULL-SNAPSHOT, latest-wins feed: the controller replaces every
// record for this (site, node) on each push. So it needs no durable outbox
// (unlike job events, which are append-only and each one matters). The feed is a
// derived shadow of live docker state (the engine + the image-check cache) — the
// source of truth lives on this host. State-persistence litmus test: "if the
// agent restarts right now, the next cycle will…" → re-push the complete snapshot
// within DOCKER_DISCOVERY_INTERVAL. No disk, no replay, correct on restart.
//
// Cadence: a periodic tick (default 5m) PLUS an out-of-band trigger fired after
// every image-check pass, so a freshly-detected outdated image surfaces within
// seconds rather than waiting for the next tick.
// ---------------------------------------------------------------------------

// DiscoveryPayload is the wire body of POST /api/docker/discovery.
type DiscoveryPayload struct {
	Site            string            `json:"site"`
	Node            string            `json:"node"`
	Containers      []ContainerStatus `json:"containers"`
	ComposeProjects []ComposeProject  `json:"compose_projects"`
	// Networks — every user-defined docker network on this host. POINTER: nil
	// means "network list failed this push, don't touch existing rows"; a non-nil
	// (even empty) slice is authoritative and replaces them. Containers/compose
	// are always authoritative so they stay plain slices.
	Networks *[]NetworkStatus `json:"networks,omitempty"`
	// TraefikNetworks — the network attachments of the host's reverse-proxy
	// container (DOCKER_PROXY_CONTAINER, default "traefik"), which a controller
	// needs to manage proxy entrypoints. Always authoritative (empty when there is
	// no such container on this host). The wire name is fixed by the controller's
	// API even though the container name is configurable.
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

	// proxyContainer is the name (or id) of the host's reverse-proxy container
	// whose network attachments are published alongside the network list.
	proxyContainer string

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
		// The proxy container is a deployment convention, not a docker fact, so it
		// is configuration with a neutral default rather than a literal in the call.
		proxyContainer: getEnv("DOCKER_PROXY_CONTAINER", "traefik"),
	}
}

// gatherNetworks returns the docker network list and the proxy container's
// network attachments, TTL-cached.
//
// Gathering is intentionally CHEAP and FIXED-COST regardless of network count:
// ONE batch NetworkList call (the summary already carries IPAM/options — no
// per-network inspect/fan-out) plus ONE ContainerInspect for the proxy container
// = 2 socket calls. There is nothing to parallelize, so unlike the image checker (which
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
	traefik := d.docker.containerNetAttachments(nctx, d.proxyContainer)
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
