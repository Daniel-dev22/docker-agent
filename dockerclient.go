package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
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

	// Image-outdated detection (Phase 3). Populated by the imageChecker's slow
	// jittered pass (NEVER inline in this snapshot) and stamped onto the container
	// during the fleet merge. Empty until the first check completes.
	ImageStatus        string `json:"image_status,omitempty"`         // updated|outdated|unknown
	LatestImageVersion string `json:"latest_image_version,omitempty"` // full name:tag of the newest available
	VersionSource      string `json:"version_source,omitempty"`       // auto:registry-digest|auto:github-release|override:…
	CurrentVersion     string `json:"current_version,omitempty"`      // running version (OCI version label or tag)
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
	// Managed is true when docker-agent owns this stack's compose files: it was
	// created via register/copy and its working dir lives under ComposeRoot, so
	// the agent can read/write it and the UI offers in-place Edit. False =
	// externally-managed (Ansible/Portainer/ad-hoc) — discovered only, files live
	// outside the agent's mount, so the UI greys out Edit. Set by
	// composeRegistry.mergeKnown when building the fleet snapshot.
	Managed bool `json:"managed,omitempty"`

	// Rolled-up image-outdated status (Phase 3), computed from the project's
	// containers during the fleet merge: outdated if ANY service is outdated.
	ImageStatus   string `json:"image_status,omitempty"` // updated|outdated|unknown
	OutdatedCount int    `json:"outdated_count,omitempty"`
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

// imageInfo is the subset of an image inspect the imageChecker needs: the local
// content digests (for digest comparison) and the OCI labels (source repo +
// running version for auto-detection).
type imageInfo struct {
	ID          string
	RepoTags    []string
	RepoDigests []string
	Labels      map[string]string
}

// inspectImage returns the local image's RepoDigests + OCI labels (Phase 3
// image-outdated detection). One inspect per UNIQUE image, run only in the
// imageChecker's slow capped pass — never in the fleet snapshot's single pass.
func (d *dockerClient) inspectImage(ctx context.Context, id string) (imageInfo, error) {
	resp, err := d.cli.ImageInspect(ctx, id)
	if err != nil {
		return imageInfo{}, err
	}
	info := imageInfo{ID: resp.ID, RepoTags: resp.RepoTags, RepoDigests: resp.RepoDigests}
	if resp.Config != nil {
		info.Labels = resp.Config.Labels
	}
	return info, nil
}

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

// ---------------------------------------------------------------------------
// Container log streaming (live follow + historical back-paging).
//
// Container logs are the agent's only EXTERNAL, daemon-owned, effectively
// unbounded log source: stdout+stderr come back stdcopy-multiplexed (raw if the
// container has a TTY), prefixed with RFC3339 timestamps, and queryable by
// Tail/Since/Until. All that Docker-specific decoding lives here; the WS
// delivery is the shared streamLines pump (wslog.go).
// ---------------------------------------------------------------------------

// containerLogChanBuf bounds the demux→WS handoff channel so the Docker reader
// isn't stalled by a slow client, without buffering unboundedly.
const containerLogChanBuf = 256

// logQuery parameterises a container log fetch. Zero-value fields are omitted
// (Tail "" means "all" to the daemon).
type logQuery struct {
	Tail       string // line count or "all"
	Since      string // RFC3339 / unix; lower time bound
	Until      string // RFC3339 / unix; upper time bound (history back-paging)
	Timestamps bool   // prefix each line with the RFC3339 emit time
	Follow     bool   // stream new lines (live tail) vs return and stop (history)
}

func (q logQuery) options() container.LogsOptions {
	return container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     q.Follow,
		Timestamps: q.Timestamps,
		Tail:       q.Tail,
		Since:      q.Since,
		Until:      q.Until,
	}
}

// lineWriter is an io.Writer that emits each complete '\n'-terminated line
// (newline stripped) to emit, retaining a partial trailing line across writes.
// It compacts in place so the buffer stays bounded by the longest single line —
// no per-line allocation beyond the emitted string, and no scanner length cap.
// Single-goroutine use only (the demux owns it).
type lineWriter struct {
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	start := 0
	for {
		i := bytes.IndexByte(w.buf[start:], '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.buf[start : start+i]))
		start += i + 1
	}
	if start > 0 {
		w.buf = w.buf[:copy(w.buf, w.buf[start:])]
	}
	return len(p), nil
}

// flush emits a final line that has no trailing newline.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = w.buf[:0]
	}
}

// demuxLogs reads a Docker log stream to EOF, emitting each line. TTY streams are
// raw; everything else is stdcopy-multiplexed (stdout+stderr to one sink keeps
// arrival order). The single decode path shared by the follow channel, the
// history slice, and containerLogsTail.
func demuxLogs(rc io.Reader, tty bool, emit func(string)) {
	lw := &lineWriter{emit: emit}
	if tty {
		_, _ = io.Copy(lw, rc)
	} else {
		_, _ = stdcopy.StdCopy(lw, lw, rc)
	}
	lw.flush()
}

// containerHasTTY reports whether the container allocates a TTY (its log stream
// is then raw, not stdcopy-multiplexed).
func (d *dockerClient) containerHasTTY(ctx context.Context, id string) (bool, error) {
	info, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return false, err
	}
	return info.Config != nil && info.Config.Tty, nil
}

// containerLogLines opens a following log stream and returns a channel of decoded
// lines plus a stop func. Docker delivers the Tail backlog inline as the first
// lines, so there is no separate backlog slice. stop() cancels the stream and
// closes the reader, unblocking the demux goroutine, which then closes the
// channel — no goroutine or stream leak on client disconnect.
func (d *dockerClient) containerLogLines(ctx context.Context, id string, q logQuery) (<-chan string, func(), error) {
	tty, err := d.containerHasTTY(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	q.Follow = true
	rc, err := d.cli.ContainerLogs(ctx, id, q.options())
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan string, containerLogChanBuf)
	streamCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(ch)
		defer rc.Close()
		demuxLogs(rc, tty, func(line string) {
			select {
			case ch <- line:
			case <-streamCtx.Done():
			}
		})
	}()
	stop := func() {
		cancel()
		_ = rc.Close()
	}
	return ch, stop, nil
}

// containerLogsHistory returns a bounded, non-following window of decoded lines
// (the viewer's back-paging and the rollback log dump both go through here).
func (d *dockerClient) containerLogsHistory(ctx context.Context, id string, q logQuery) ([]string, error) {
	tty, err := d.containerHasTTY(ctx, id)
	if err != nil {
		return nil, err
	}
	q.Follow = false
	rc, err := d.cli.ContainerLogs(ctx, id, q.options())
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var lines []string
	demuxLogs(rc, tty, func(line string) { lines = append(lines, line) })
	return lines, nil
}

// ---------------------------------------------------------------------------
// Inspect helpers (Phase 3.5 stack-update engine: health-wait + net reconcile).
// One ContainerInspect per container, used ONLY in the update pipeline (a
// deliberate op), never in the fleet snapshot's single ContainerList pass.
// ---------------------------------------------------------------------------

// containerState is the slice of a container inspect the health-wait needs —
// the Go equivalent of docker_health_wait.py's
// `Id|health|restart_count|running` format string (no subprocess, no parsing).
type containerState struct {
	ID           string
	Health       string // healthy|unhealthy|starting|none
	RestartCount int
	Running      bool
}

// inspectState returns a container's id, health status, restart count, and
// running flag. A missing container surfaces as an error (the caller treats it
// as "being recreated" during swap detection, exactly like the python).
func (d *dockerClient) inspectState(ctx context.Context, nameOrID string) (containerState, error) {
	resp, err := d.cli.ContainerInspect(ctx, nameOrID)
	if err != nil {
		return containerState{}, err
	}
	st := containerState{ID: resp.ID, Health: "none", RestartCount: resp.RestartCount}
	if resp.State != nil {
		st.Running = resp.State.Running
		if resp.State.Health != nil && resp.State.Health.Status != "" {
			st.Health = resp.State.Health.Status
		}
	}
	return st, nil
}

// containerNetworks returns the set of network names a container is currently
// attached to (Phase 3.5 net reconcile: detect containers detached from a
// recreated network).
func (d *dockerClient) containerNetworks(ctx context.Context, nameOrID string) (map[string]struct{}, error) {
	resp, err := d.cli.ContainerInspect(ctx, nameOrID)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	if resp.NetworkSettings != nil {
		for name := range resp.NetworkSettings.Networks {
			out[name] = struct{}{}
		}
	}
	return out, nil
}

// containerLogsTail returns the last `tail` lines of a container's combined
// stdout+stderr (Phase 3.5: dump an unhealthy container's logs into the update
// job log on rollback). Non-following, no timestamps, empty lines dropped — a
// thin filter over the shared historical-window fetch, byte-compatible with its
// previous behaviour (TTY handling now via authoritative inspect, not a retry).
func (d *dockerClient) containerLogsTail(ctx context.Context, nameOrID, tail string) ([]string, error) {
	raw, err := d.containerLogsHistory(ctx, nameOrID, logQuery{Tail: tail})
	if err != nil {
		return nil, err
	}
	lines := raw[:0]
	for _, ln := range raw {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	return lines, nil
}
