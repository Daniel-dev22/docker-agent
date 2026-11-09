package main

// Process-global pooled HTTP client for every agent → controller call (job event
// push via the kit outbox, reconcile, discovery, GitHub-token vend).
//
// Auth model (shared with the sibling agents): transport mTLS is handled at the
// reverse-proxy boundary on each side; the agent presents NO client cert.
// Per-node identity is the BearerToken header attached by the kit bearer round-tripper (agent-kit-go/transport), which also refuses
// cross-origin redirects so the token and body never follow one.

import (
	"net/http"

	"github.com/Daniel-dev22/agent-kit-go/transport"
)

// buildControlCenterClient returns the pooled HTTP client. Direct mode
// (TraefikDockerDNS=="") dials the URL host normally; rewrite mode
// (TraefikDockerDNS!="") rewrites the dial TARGET to the host's local Traefik
// while keeping the URL host as the Host header and TLS SNI, so that proxy can
// match its routing rule and attach the mTLS client identity on the way out.
func buildControlCenterClient(cfg Config) *http.Client {
	return transport.NewRouterClient(transport.RouterClientConfig{
		TraefikDockerDNS: cfg.TraefikDockerDNS,
		TraefikDialPort:  cfg.TraefikDialPort,
		BearerToken:      cfg.BearerToken,
	})
}
