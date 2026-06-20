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
// The agent reaches controller exactly like duplicacy/gdrive/build: transport
// mTLS is attached by the local docker Traefik (NAS) or k3s Traefik (cluster
// node); app-layer identity is the per-host BearerToken. It NEVER holds a client
// cert. The docker socket is mounted (never on TCP) — root-equivalent, the
// standard Portainer-agent posture (we replace the holder, not widen trust).
type Config struct {
	NodeName string // host class this agent runs on (e.g. "nas", "nuc", "pi", "vm", "nuc02")
	SiteID   string // "kd" | "ng"

	ConfigDir string // persistent state dir (events.sqlite, heartbeat) — bind-mounted

	// Docker engine + compose state.
	DockerHost string // docker endpoint (default unix:///var/run/docker.sock)
	// ComposeRoot is the BIND-MOUNTED agent data dir (host==container path) under
	// which this node's compose stacks live (<ComposeRoot>/<stack>/). Phase 2 uses
	// it; declared here so the contract is stable from Phase 0.
	ComposeRoot string
	// ComposeRegistryPath is the durable projects.json (Phase 2 source of truth) —
	// under ComposeRoot so it survives restarts (the "output must be bind-mounted"
	// lesson), NOT container-internal /var/lib.
	ComposeRegistryPath string
	// RegistryAuthFile is the mounted ~/.docker/config.json for private-registry
	// digest checks (Phase 3). Declared early for a stable env contract.
	RegistryAuthFile string

	ControlCenterURL string // https://controller-api.<domain>:1443
	TraefikDockerDNS string // docker DNS name → local Traefik ("traefik" on NAS; "" = direct on k3s)
	TraefikDialPort  string // port on the resolved Traefik IP (default "1443")

	BearerTokenFile string // per-host bearer token file (mode 0600, bind-mounted)
	BearerToken     string // populated at startup from BearerTokenFile
}

func loadConfig() Config {
	cfg := Config{
		NodeName: requireEnv("NODE_NAME"),
		SiteID:   requireEnv("SITE_ID"),

		ConfigDir: getEnv("CONFIG_DIR", "/var/lib/docker-agent"),

		DockerHost:       getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		ComposeRoot:      getEnv("COMPOSE_ROOT", "/srv/containers/docker-agent/data"),
		RegistryAuthFile: getEnv("REGISTRY_AUTH_FILE", ""),
		ControlCenterURL: requireEnv("CONTROL_CENTER_URL"),
		// Empty default — k3s nodes use direct mode; NAS hosts set "traefik" to
		// opt into rewrite mode (see network.go). A non-empty default would break
		// k3s nodes the way it did for the other agents.
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
