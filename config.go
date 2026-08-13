package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the docker-agent runtime config, loaded from env at startup.
//
// The agent reaches the controller the way every sibling agent does: transport
// mTLS is terminated by the reverse proxy in front of it on each side, and
// app-layer identity is the per-host BearerToken. It NEVER holds a client cert.
// The docker socket is mounted (never exposed on TCP) — root-equivalent, the
// standard container-agent posture.
type Config struct {
	NodeName string // node identity within the site (e.g. "nas01", "nuc01", "pi01"),
	// derived from the container hostname ("<site>-<node>") minus the SiteID prefix.
	SiteID string // deployment site identifier, e.g. "site-a" | "site-b"

	ConfigDir string // persistent state dir (events.sqlite, heartbeat) — bind-mounted

	// Docker engine + compose state.
	DockerHost string // docker endpoint (default unix:///var/run/docker.sock)
	// ComposeRoot is the BIND-MOUNTED agent data dir under which this node's
	// compose stacks live (<ComposeRoot>/<stack>/). The host path and the container
	// path MUST be identical: compose records host-resolved paths in its labels and
	// writes them back, so a rewritten path would point at nothing on the host.
	ComposeRoot string
	// ComposeRegistryPath is the durable projects.json — the source of truth for
	// which projects exist. It lives under ComposeRoot so it survives restarts,
	// NOT in a container-internal /var/lib.
	ComposeRegistryPath string
	// RegistryAuthFile is the mounted ~/.docker/config.json used for private-registry
	// digest checks.
	RegistryAuthFile string

	ControlCenterURL string // https://<controller-host>:1443
	TraefikDockerDNS string // docker DNS name of the host's local proxy ("traefik"); "" = dial the URL host directly
	TraefikDialPort  string // port on the resolved Traefik IP (default "1443")

	BearerTokenFile string // per-host bearer token file (mode 0600, bind-mounted)
	BearerToken     string // populated at startup from BearerTokenFile
}

func loadConfig() Config {
	site := requireEnv("SITE_ID")
	cfg := Config{
		// The node identity is the container hostname ("<site>-<node>", e.g.
		// "site-a-pi01") minus the "<site>-" prefix → "pi01". This is the SINGLE
		// source of truth — there is no NODE_NAME env (the hostname already encodes
		// it, and it must match the per-host name the rest of the system uses so a
		// host has ONE identity across every inventory source).
		NodeName: deriveNodeName(site),
		SiteID:   site,

		ConfigDir: getEnv("CONFIG_DIR", "/var/lib/docker-agent"),

		DockerHost:       getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		ComposeRoot:      getEnv("COMPOSE_ROOT", "/srv/containers/docker-agent/data"),
		RegistryAuthFile: getEnv("REGISTRY_AUTH_FILE", ""),
		ControlCenterURL: requireEnv("CONTROL_CENTER_URL"),
		// Empty default — a host that can reach the controller directly leaves this
		// unset; a host that must egress through its own local Traefik sets "traefik"
		// to opt into rewrite mode (see network.go). A non-empty default would break
		// every direct-dial host.
		TraefikDockerDNS: getEnv("TRAEFIK_DOCKER_DNS", ""),
		TraefikDialPort:  getEnv("TRAEFIK_DIAL_PORT", "1443"),

		BearerTokenFile: getEnv("BEARER_TOKEN_FILE", "/etc/docker-agent/bearer-token"),
	}
	cfg.ComposeRegistryPath = getEnv("COMPOSE_REGISTRY_PATH", cfg.ComposeRoot+"/projects.json")

	tok, err := readBearerToken(cfg.BearerTokenFile)
	if err != nil {
		slog.Error("bearer token unreadable — cannot authenticate to controller",
			"error", err, "path", cfg.BearerTokenFile)
		os.Exit(1)
	}
	cfg.BearerToken = tok
	return cfg
}

func readBearerToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", &emptyTokenErr{path: path}
	}
	return tok, nil
}

type emptyTokenErr struct{ path string }

func (e *emptyTokenErr) Error() string { return "bearer token file " + e.path + " is empty" }

// deriveNodeName resolves this agent's node identity from the container hostname
// ("<site>-<node>") by stripping the "<site>-" prefix — e.g. "site-a-pi01" →
// "pi01". It mirrors the prefix strip the controller does when it routes a call
// to a node, so the two always agree. The hostname is REQUIRED (the deployment
// sets it); a hostname that doesn't carry the site prefix is a misconfiguration
// we refuse to start on rather than silently report a wrong/ambiguous node name.
func deriveNodeName(site string) string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		slog.Error("cannot read container hostname for node identity", "error", err)
		os.Exit(1)
	}
	host = strings.TrimSpace(host)
	prefix := site + "-"
	if !strings.HasPrefix(host, prefix) {
		slog.Error("container hostname does not start with the site prefix — cannot derive node name",
			"hostname", host, "site", site)
		os.Exit(1)
	}
	node := strings.TrimPrefix(host, prefix)
	if node == "" {
		slog.Error("derived empty node name from hostname", "hostname", host, "site", site)
		os.Exit(1)
	}
	return node
}

func requireEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("required env var not set", "var", k)
		os.Exit(1)
	}
	return v
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getEnvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid int env var, using default", "var", k, "value", v, "default", def)
		return def
	}
	return n
}

func getEnvDuration(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env var, using default", "var", k, "value", v, "default", def)
		return def
	}
	return d
}
