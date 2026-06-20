package main

import (
	"context"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Compose labels written by docker compose on every managed container.
const (
	labelComposeProject    = "com.docker.compose.project"
	labelComposeService    = "com.docker.compose.service"
	labelComposeWorkingDir = "com.docker.compose.project.working_dir"
	labelComposeConfigFile = "com.docker.compose.project.config_files"
)

var exitCodeRe = regexp.MustCompile(`Exited \((\d+)\)`)

// Port is one published port mapping (mirrors the docker `Port` summary shape,
// snake_cased for the JSON wire contract).
type Port struct {
	IP          string `json:"ip,omitempty"`
	PrivatePort uint16 `json:"private_port"`
	PublicPort  uint16 `json:"public_port,omitempty"`
	Type        string `json:"type"`
}

// ContainerStatus is the read-only per-container row the dashboard renders. It is
// assembled from a SINGLE ContainerList call (no per-container inspect) — health
// and exit code are parsed from the Status string (exactly what `docker ps`
// shows), keeping fleet collection a bounded single pass (the Pi-safe posture).
type ContainerStatus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Image          string `json:"image"`
	ImageID        string `json:"image_id"`
	State          string `json:"state"`  // running|exited|created|paused|restarting|dead|removing
	Status         string `json:"status"` // human "Up 3 hours (healthy)"
	Health         string `json:"health"` // healthy|unhealthy|starting|none
	Created        int64  `json:"created"`
	ExitCode       int    `json:"exit_code,omitempty"`
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`
	Ports          []Port `json:"ports,omitempty"`
}

// ComposeProject groups containers sharing a compose project label. Phase 0
// derives this purely from RUNNING/known container labels; Phase 2 adds the
// durable projects.json registry so stopped-but-known projects also appear.
type ComposeProject struct {
	Name           string   `json:"name"`
	WorkingDir     string   `json:"working_dir,omitempty"`
	ConfigFiles    string   `json:"config_files,omitempty"`
	ContainerCount int      `json:"container_count"`
	RunningCount   int      `json:"running_count"`
	Containers     []string `json:"containers"`
}

// dockerClient wraps the moby Engine API client. Phase 0 uses it read-only
// (listContainers); container lifecycle ops (start/stop/restart/remove,
// ContainerLogs follow) land in Phase 1, image digest inspect in Phase 3.
type dockerClient struct {
	cli           *client.Client
	serverVersion string // cached once at construction (rarely changes)
}

func newDockerClient(host string) (*dockerClient, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	} else {
		opts = append(opts, client.FromEnv)
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	dc := &dockerClient{cli: cli}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v, err := cli.ServerVersion(ctx); err == nil {
		dc.serverVersion = v.Version
	}
	return dc, nil
}

func (d *dockerClient) close() error { return d.cli.Close() }

// snapshot returns the full container list plus the compose-project grouping in a
// single Engine API call — the bounded single-pass collection the plan requires.
func (d *dockerClient) snapshot(ctx context.Context) ([]ContainerStatus, []ComposeProject, error) {
	summaries, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, nil, err
	}
	containers := make([]ContainerStatus, 0, len(summaries))
	for _, s := range summaries {
		containers = append(containers, summaryToStatus(s))
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, groupComposeProjects(summaries), nil
}

func summaryToStatus(s container.Summary) ContainerStatus {
	name := ""
	if len(s.Names) > 0 {
		name = strings.TrimPrefix(s.Names[0], "/")
	}
	cs := ContainerStatus{
		ID:             shortID(s.ID),
		Name:           name,
		Image:          s.Image,
		ImageID:        s.ImageID,
		State:          s.State,
		Status:         s.Status,
		Health:         parseHealth(s.Status),
		Created:        s.Created,
		ComposeProject: s.Labels[labelComposeProject],
		ComposeService: s.Labels[labelComposeService],
	}
	if s.State == "exited" || s.State == "dead" {
		if m := exitCodeRe.FindStringSubmatch(s.Status); m != nil {
			cs.ExitCode, _ = strconv.Atoi(m[1])
		}
	}
	for _, p := range s.Ports {
		cs.Ports = append(cs.Ports, Port{IP: p.IP, PrivatePort: p.PrivatePort, PublicPort: p.PublicPort, Type: p.Type})
	}
	return cs
}

// groupComposeProjects buckets containers by their compose project label,
// counting total + running and capturing the working_dir / config_files (set
// identically on every container of a project, so first-seen wins).
func groupComposeProjects(summaries []container.Summary) []ComposeProject {
	byName := map[string]*ComposeProject{}
	for _, s := range summaries {
		proj := s.Labels[labelComposeProject]
		if proj == "" {
			continue
		}
		p := byName[proj]
		if p == nil {
			p = &ComposeProject{
				Name:        proj,
				WorkingDir:  s.Labels[labelComposeWorkingDir],
				ConfigFiles: s.Labels[labelComposeConfigFile],
			}
			byName[proj] = p
		}
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		p.Containers = append(p.Containers, name)
		p.ContainerCount++
		if s.State == "running" {
			p.RunningCount++
		}
	}
	out := make([]ComposeProject, 0, len(byName))
	for _, p := range byName {
		sort.Strings(p.Containers)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// parseHealth extracts the healthcheck state from the `docker ps` Status string
// (e.g. "Up 4 hours (healthy)") — avoids a per-container inspect.
func parseHealth(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	default:
		return "none"
	}
}

// ---------------------------------------------------------------------------
// Container lifecycle ops (Phase 1).
//
// Each is a thin typed wrapper over the moby Engine API — no shell, no output
// parsing. They are driven as short async jobs by the engine (start has no
// grace, but stop/restart/remove-running carry a SIGTERM grace, so none are
// treated as synchronous). The container id may be the short id the snapshot
// publishes; the Engine API resolves it by prefix.
// ---------------------------------------------------------------------------

func (d *dockerClient) startContainer(ctx context.Context, id string) error {
	return d.cli.ContainerStart(ctx, id, container.StartOptions{})
}

// stopContainer sends SIGTERM then SIGKILL after the grace (timeout seconds;
// nil = the engine/image default).
func (d *dockerClient) stopContainer(ctx context.Context, id string, timeout *int) error {
	return d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: timeout})
}

func (d *dockerClient) restartContainer(ctx context.Context, id string, timeout *int) error {
	return d.cli.ContainerRestart(ctx, id, container.StopOptions{Timeout: timeout})
}

// killContainer sends SIGKILL immediately (the bulk "kill" action — no grace).
func (d *dockerClient) killContainer(ctx context.Context, id string) error {
	return d.cli.ContainerKill(ctx, id, "KILL")
}

// removeContainer removes a container; force kills a running one first.
func (d *dockerClient) removeContainer(ctx context.Context, id string, force bool) error {
	return d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

// containerLogsFollow opens a following log stream (stdout+stderr, timestamps,
// last 500 lines of backlog). The caller owns Close. Multiplexed stdcopy frames
// are demuxed by the reader in engine/handlers; raw text otherwise.
func (d *dockerClient) containerLogsFollow(ctx context.Context, id string) (io.ReadCloser, error) {
	return d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
		Tail:       "500",
	})
}
