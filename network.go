package main

// Process-global pooled HTTP client for every agent → controller call
// (event push via the kit outbox, reconcile).
//
// Auth model (identical to duplicacy/gdrive/build): transport mTLS is handled at
// the Traefik boundary on each side; the agent presents NO client cert. Per-node
// identity is the BearerToken header attached by the round-tripper below.
//
// (No GitHub-token vend here — that is build-agent's concern. Image-status
// version polling in Phase 3 will reuse build-agent's GitHub App token vend via
// the controller, not a local copy.)

import (
	"context"
	"net"
	"net/http"
	"time"
)

// bearerAuthRoundTripper attaches the per-host bearer token to every outgoing
// request (unless the caller already set Authorization).
type bearerAuthRoundTripper struct {
	rt    http.RoundTripper
	token string
}

func (b *bearerAuthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" && r.Header.Get("Authorization") == "" {
		r2 := r.Clone(r.Context())
		r2.Header.Set("Authorization", "Bearer "+b.token)
		return b.rt.RoundTrip(r2)
	}
	return b.rt.RoundTrip(r)
}

// buildControlCenterClient returns the pooled HTTP client. Direct mode (k3s,
// TraefikDockerDNS=="") dials the URL host normally; Traefik-rewrite mode (NAS,
// TraefikDockerDNS!="") rewrites the dial target to the local docker Traefik
// while keeping the URL host as TLS SNI, so the host's mTLS ServersTransport
// attaches the cluster identity.
func buildControlCenterClient(cfg Config) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     0,
	}
	if cfg.TraefikDockerDNS != "" {
		traefikTarget := net.JoinHostPort(cfg.TraefikDockerDNS, cfg.TraefikDialPort)
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, traefikTarget)
		}
	} else {
		transport.DialContext = dialer.DialContext
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &bearerAuthRoundTripper{rt: transport, token: cfg.BearerToken},
	}
}
