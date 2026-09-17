package main

// Self identity — which container and compose project is THIS agent.
//
// The agent drives compose and container ops through the host's Docker daemon, so
// nothing structural stops it from being pointed at itself. It must never act on
// itself: compose stops (or removes as an orphan) the container running the job,
// the process dies mid-operation, no replacement is created, and the host is left
// with no agent at all — recoverable only by whatever provisioned it. So the agent
// learns its own identity and refuses those targets up front.
//
// The container ID comes from /proc/self/mountinfo: Docker bind-mounts
// /etc/hostname, /etc/hosts and /etc/resolv.conf from
// <data-root>/containers/<64-hex id>/, whatever the data root is. That read is
// local and cannot fail on a Docker outage. The compose project needs one inspect
// of that ID; it is resolved lazily and retried, so a daemon that was briefly
// unreachable at boot does not leave the guard unarmed for the pod's lifetime.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// selfResolveRetry bounds how often a failed project resolution is retried. The
// fleet snapshot asks on every build; a daemon that is down must not be inspected
// on every one of them.
const selfResolveRetry = 30 * time.Second

var containerIDInMountRoot = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// dockerSetupMounts are the per-container files Docker bind-mounts from the
// container's own state directory. Only these lines are trusted: an operator's
// bind mount whose host path happens to contain /containers/<hex>/ is not proof of
// identity.
var dockerSetupMounts = map[string]bool{
	"/etc/hostname":    true,
	"/etc/hosts":       true,
	"/etc/resolv.conf": true,
}

// parseSelfContainerID extracts this process's container ID from a mountinfo
// stream (proc(5): field 4 is the mount root, field 5 the mount point). Lines too
// short to carry both are skipped. Two different IDs is an error, not a guess.
func parseSelfContainerID(r io.Reader) (string, error) {
	var found string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !dockerSetupMounts[fields[4]] {
			continue
		}
		m := containerIDInMountRoot.FindStringSubmatch(fields[3])
		if m == nil {
			continue
		}
		if found != "" && found != m[1] {
			return "", fmt.Errorf("mountinfo names two containers (%s, %s)", shortID(found), shortID(m[1]))
		}
		found = m[1]
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read mountinfo: %w", err)
	}
	if found == "" {
		return "", errors.New("no docker container mounts in mountinfo (not running in a docker container?)")
	}
	return found, nil
}

// containerInspector is the one Docker call self identity needs. dockerClient
// satisfies it; tests supply a fake.
type containerInspector interface {
	containerIdentity(ctx context.Context, nameOrID string) (id, name string, labels map[string]string, err error)
}

// selfIdentity holds the agent's own container ID (fixed at boot) and its compose
// project + container name (resolved lazily).
type selfIdentity struct {
	containerID string // full 64-hex; "" when idErr != nil
	idErr       error  // permanent: mountinfo is not going to change

	inspector containerInspector

	mu          sync.Mutex
	resolved    bool
	project     string
	name        string // container name without the leading "/"
	lastAttempt time.Time
	lastErr     error
}

// newSelfIdentity reads the container ID from mountinfoPath and makes one bounded
// attempt at the compose project. Failures are logged once here; resolve logs only
// on a state change after that.
func newSelfIdentity(ctx context.Context, mountinfoPath string, inspector containerInspector) *selfIdentity {
	s := &selfIdentity{inspector: inspector}
	f, err := os.Open(mountinfoPath)
	if err == nil {
		s.containerID, s.idErr = parseSelfContainerID(f)
		_ = f.Close()
	} else {
		s.idErr = fmt.Errorf("open %s: %w", mountinfoPath, err)
	}
	if s.idErr != nil {
		slog.Error("self identity unresolved — refusing ops on this agent's own container is disabled; "+
			"compose ops on its stack stay refused only by the compose-root rule", "error", s.idErr)
		return s
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s.projectName(actx)
	if s.lastErr != nil {
		slog.Warn("self compose project unresolved at boot — will retry", "container", shortID(s.containerID), "error", s.lastErr)
	} else {
		slog.Info("self identity resolved", "container", shortID(s.containerID), "name", s.name, "project", s.project)
	}
	return s
}

// projectName returns this agent's compose project ("" when unknown or when the
// container was not started by compose). A failed resolution is retried at most
// once per selfResolveRetry.
func (s *selfIdentity) projectName(ctx context.Context) string {
	if s == nil || s.containerID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved || s.inspector == nil {
		return s.project
	}
	if !s.lastAttempt.IsZero() && time.Since(s.lastAttempt) < selfResolveRetry {
		return ""
	}
	s.lastAttempt = time.Now()
	_, name, labels, err := s.inspector.containerIdentity(ctx, s.containerID)
	if err != nil {
		s.lastErr = err
		return ""
	}
	if s.lastErr != nil && !s.lastAttempt.IsZero() {
		slog.Info("self compose project resolved after retry", "container", shortID(s.containerID))
	}
	s.resolved, s.lastErr = true, nil
	s.name = strings.TrimPrefix(name, "/")
	s.project = labels[labelComposeProject]
	return s.project
}

// isSelfContainer reports whether a container reference — full ID, ID prefix or
// name, exactly the forms the Engine API accepts — names this agent. A hex prefix
// of the agent's ID is treated as self even when short: Docker resolves a prefix
// only when it is unique, so a prefix shared with the agent is either the agent or
// ambiguous, and refusing both is correct.
func (s *selfIdentity) isSelfContainer(ctx context.Context, target string) bool {
	if s == nil || s.containerID == "" {
		return false
	}
	t := strings.TrimPrefix(strings.TrimSpace(target), "/")
	if t == "" {
		return false
	}
	if isLowerHex(t) && strings.HasPrefix(s.containerID, t) {
		return true
	}
	s.projectName(ctx) // fills s.name when it can
	s.mu.Lock()
	name, inspector := s.name, s.inspector
	s.mu.Unlock()
	if name != "" {
		return t == name
	}
	// Name still unknown: ask the daemon what the target resolves to. An inspect
	// error means the op will fail on its own, so it is not treated as self.
	if inspector == nil {
		return false
	}
	ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	id, _, _, err := inspector.containerIdentity(ictx, t)
	return err == nil && id == s.containerID
}

// status is the readiness-payload view: what the guard is keyed on, or why it is
// not armed.
func (s *selfIdentity) status() map[string]any {
	out := map[string]any{}
	if s == nil {
		out["error"] = "self identity not initialised"
		return out
	}
	if s.idErr != nil {
		out["error"] = s.idErr.Error()
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out["container_id"] = shortID(s.containerID)
	if s.resolved {
		out["name"] = s.name
		out["project"] = s.project
	} else if s.lastErr != nil {
		out["error"] = "compose project unresolved: " + s.lastErr.Error()
	}
	return out
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}
