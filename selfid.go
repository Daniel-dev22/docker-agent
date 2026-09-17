package main

// Self identity — which containers and compose projects are THIS agent, and which
// container carries its control path.
//
// The agent drives compose and container ops through the host's Docker daemon, so
// nothing structural stops it from being pointed at itself. It must never act on
// itself: compose stops (or removes as an orphan) the container running the job,
// the process dies mid-operation, no replacement is created, and the host is left
// with no agent at all — recoverable only by whatever provisioned it.
//
// The container ID comes from /proc/self/mountinfo: Docker bind-mounts
// /etc/hostname, /etc/hosts and /etc/resolv.conf from
// <data-root>/containers/<64-hex id>/, whatever the data root is. That read is
// local and cannot fail on a Docker outage.
//
// Everything else is derived from a container LIST, never from a separate inspect:
// the list is the call every snapshot and every mutating handler already makes, so
// there is no second, throttled resolution whose failure window leaves the guard
// open, and nothing is ever looked up while a lock is held. Each derivation is
// published whole through an atomic pointer, so readiness reads it without waiting
// on the daemon.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
)

var containerIDInMountRoot = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// mountinfoPath is where the agent reads its own mounts. A variable only so the
// app wiring can be exercised against a fixture.
var mountinfoPath = "/proc/self/mountinfo"

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

// selfView is one derivation of self identity from one container list. Immutable
// once published.
type selfView struct {
	// observedAt is when the list it was derived from STARTED: a slower list that
	// began earlier describes an older state, however late it returns.
	observedAt time.Time
	// found: the container mountinfo names was in the list.
	found bool
	name  string
	// ids are the full IDs treated as self: the mountinfo container plus every
	// container sharing its network namespace (network_mode container:<it>). Docker
	// copies the network OWNER's hostname/hosts/resolv.conf into a dependent, so
	// when the agent is the dependent, mountinfo names the owner — and the agent is
	// one of these dependents.
	ids map[string]struct{}
	// projects are the compose projects of every container in ids.
	projects map[string]struct{}
	// ambiguous names the network-namespace dependents: when non-empty the agent
	// cannot tell which of these containers it is, so all of them are self.
	ambiguous []string
	// controlPath is the container the agent's control-center traffic dials
	// (TRAEFIK_DOCKER_DNS), matched by exact container name. Stopping it leaves the
	// agent alive but unreachable, with nothing able to start it again. The name is
	// the only thing a list can match on: the Engine API's container list returns
	// no network aliases or DNS names (null on every network), so an alias match
	// would silently never fire.
	controlPath    *controlPathContainer
	controlPathErr string
}

type controlPathContainer struct {
	id, name, project string
}

func (v *selfView) isSelfProject(name string) bool {
	if v == nil || name == "" {
		return false
	}
	_, ok := v.projects[name]
	return ok
}

func (v *selfView) isSelfID(fullID string) bool {
	if v == nil || fullID == "" {
		return false
	}
	_, ok := v.ids[fullID]
	return ok
}

func (v *selfView) isControlPathProject(name string) bool {
	return v != nil && v.controlPath != nil && name != "" && v.controlPath.project == name
}

func (v *selfView) isControlPathID(fullID string) bool {
	return v != nil && v.controlPath != nil && fullID != "" && v.controlPath.id == fullID
}

// deriveSelfView computes self identity from one container list that started at
// startedAt. selfID may be "" (mountinfo unreadable): then nothing is self, but the
// control path is still derived — it needs only the list.
func deriveSelfView(selfID, controlPathName string, summaries []container.Summary, startedAt time.Time) *selfView {
	v := &selfView{observedAt: startedAt, ids: map[string]struct{}{}, projects: map[string]struct{}{}}
	var ownNames []string
	if selfID != "" {
		v.ids[selfID] = struct{}{}
		for _, s := range summaries {
			if s.ID != selfID {
				continue
			}
			v.found = true
			ownNames = summaryNames(s)
			if len(ownNames) > 0 {
				v.name = ownNames[0]
			}
			if p := s.Labels[labelComposeProject]; p != "" {
				v.projects[p] = struct{}{}
			}
		}
		// Every container in the mountinfo container's network namespace. The mode
		// names the owner by ID (compose `service:` and most runtimes) or by name
		// (`docker run --network container:<name>`).
		owners := map[string]bool{"container:" + selfID: true}
		for _, n := range ownNames {
			owners["container:"+n] = true
		}
		for _, s := range summaries {
			if s.ID == selfID || !owners[s.HostConfig.NetworkMode] {
				continue
			}
			v.ids[s.ID] = struct{}{}
			if names := summaryNames(s); len(names) > 0 {
				v.ambiguous = append(v.ambiguous, names[0])
			} else {
				v.ambiguous = append(v.ambiguous, shortID(s.ID))
			}
			if p := s.Labels[labelComposeProject]; p != "" {
				v.projects[p] = struct{}{}
			}
		}
		sort.Strings(v.ambiguous)
	}
	if controlPathName != "" {
		for _, s := range summaries {
			if !hasName(s, controlPathName) {
				continue
			}
			v.controlPath = &controlPathContainer{id: s.ID, name: controlPathName, project: s.Labels[labelComposeProject]}
			break
		}
		if v.controlPath == nil {
			v.controlPathErr = fmt.Sprintf("no container named %q", controlPathName)
		}
	}
	return v
}

func hasName(s container.Summary, name string) bool {
	for _, n := range summaryNames(s) {
		if n == name {
			return true
		}
	}
	return false
}

func summaryNames(s container.Summary) []string {
	out := make([]string, 0, len(s.Names))
	for _, n := range s.Names {
		if n = strings.TrimPrefix(n, "/"); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// selfIdentity holds the agent's own container ID (fixed at boot: a restart keeps
// the ID, a recreate is a new process) and the latest derived view.
type selfIdentity struct {
	containerID     string // full 64-hex; "" when idErr != nil
	idErr           error  // permanent: mountinfo is not going to change
	controlPathName string

	view        atomic.Pointer[selfView]
	lastListErr atomic.Pointer[listFailure]

	logMu   sync.Mutex // guards lastLog only; never held across I/O
	lastLog string
}

// newSelfIdentity reads the container ID from path. It makes no Docker call: the
// rest of the identity arrives with the first container list.
func newSelfIdentity(path, controlPathName string) *selfIdentity {
	s := &selfIdentity{controlPathName: controlPathName}
	f, err := os.Open(path)
	if err == nil {
		s.containerID, s.idErr = parseSelfContainerID(f)
		_ = f.Close()
	} else {
		s.idErr = err // *PathError already names the file
	}
	if s.idErr != nil {
		slog.Error("self identity unresolved — ops on this agent's own container and stack cannot be refused",
			"error", s.idErr)
	}
	return s
}

// listFailure is a container list that failed, stamped with when it started.
type listFailure struct {
	startedAt time.Time
	msg       string
}

// known reports whether the agent knows which container it is.
func (s *selfIdentity) known() bool { return s != nil && s.containerID != "" }

// protects reports whether anything is protected by what a container list shows:
// the agent's own containers (needs the mountinfo ID) or its control-path proxy
// (needs only the configured name). A request that cannot see the list while
// either holds cannot be proven safe.
func (s *selfIdentity) protects() bool {
	return s != nil && (s.containerID != "" || s.controlPathName != "")
}

// observe derives a view from a container list that started at startedAt and
// publishes it unless a list that started later has already published. The view
// derived from THIS list is returned either way: a caller decides on the
// containers it saw.
func (s *selfIdentity) observe(summaries []container.Summary, startedAt time.Time) *selfView {
	if s == nil {
		return nil
	}
	v := deriveSelfView(s.containerID, s.controlPathName, summaries, startedAt)
	for {
		old := s.view.Load()
		if old != nil && old.observedAt.After(startedAt) {
			return v
		}
		if s.view.CompareAndSwap(old, v) {
			break
		}
	}
	// A success clears only a failure that started no later than it did.
	for {
		f := s.lastListErr.Load()
		if f == nil || f.startedAt.After(startedAt) || s.lastListErr.CompareAndSwap(f, nil) {
			break
		}
	}
	s.logTransition(v)
	return v
}

// observeFailed records a container-list failure (the list started at startedAt)
// for readiness, unless a list that started later has already succeeded or
// failed. The previous view stays published: it is still the best description of
// what the agent last saw.
func (s *selfIdentity) observeFailed(err error, startedAt time.Time) {
	if s == nil || err == nil {
		return
	}
	f := &listFailure{startedAt: startedAt, msg: err.Error()}
	if v := s.view.Load(); v != nil && v.observedAt.After(startedAt) {
		return
	}
	for {
		old := s.lastListErr.Load()
		if old != nil && old.startedAt.After(startedAt) {
			return
		}
		if s.lastListErr.CompareAndSwap(old, f) {
			return
		}
	}
}

// current is the last published view (nil before the first list).
func (s *selfIdentity) current() *selfView {
	if s == nil {
		return nil
	}
	return s.view.Load()
}

// logTransition logs the first view and every later change of what is protected,
// never the same state twice.
func (s *selfIdentity) logTransition(v *selfView) {
	cp := ""
	if v.controlPath != nil {
		cp = v.controlPath.name + "/" + v.controlPath.project
	}
	key := fmt.Sprintf("%t|%s|%v|%v|%s|%s", v.found, v.name, sortedKeys(v.projects), v.ambiguous, cp, v.controlPathErr)
	s.logMu.Lock()
	changed := key != s.lastLog
	s.lastLog = key
	s.logMu.Unlock()
	if !changed {
		return
	}
	switch {
	case s.containerID != "" && !v.found:
		slog.Error("self identity: own container is not in the container list — only its ID is protected",
			"container", shortID(s.containerID))
	case len(v.ambiguous) > 0:
		slog.Warn("self identity is ambiguous — containers share this agent's network namespace; all are protected",
			"container", shortID(s.containerID), "shared_with", v.ambiguous, "projects", sortedKeys(v.projects))
	case s.containerID != "":
		slog.Info("self identity resolved", "container", shortID(s.containerID), "name", v.name, "projects", sortedKeys(v.projects))
	}
	if v.controlPathErr != "" {
		slog.Warn("control-path container not found — nothing protects it from being stopped", "error", v.controlPathErr)
	} else if v.controlPath != nil {
		slog.Info("control-path container identified", "name", v.controlPath.name, "project", v.controlPath.project)
	}
}

// status is the readiness-payload view: what the guards are keyed on, or why they
// are not. It never blocks.
func (s *selfIdentity) status() map[string]any {
	out := map[string]any{}
	if s == nil {
		out["error"] = "self identity not initialised"
		return out
	}
	if s.idErr != nil {
		out["error"] = s.idErr.Error()
	} else {
		out["container_id"] = shortID(s.containerID)
	}
	if f := s.lastListErr.Load(); f != nil {
		out["last_list_error"] = f.msg
	}
	v := s.view.Load()
	if v == nil {
		if s.idErr == nil {
			out["error"] = "not yet observed: no container list has succeeded"
		}
		return out
	}
	out["observed_at"] = v.observedAt.UTC().Format(time.RFC3339)
	if s.containerID != "" {
		if v.found {
			out["name"] = v.name
			out["projects"] = sortedKeys(v.projects)
		} else {
			out["error"] = "own container is not in the container list"
		}
	}
	if len(v.ambiguous) > 0 {
		out["ambiguous"] = v.ambiguous
	}
	if s.controlPathName != "" {
		if v.controlPath != nil {
			out["control_path"] = map[string]any{
				"name": v.controlPath.name, "container_id": shortID(v.controlPath.id), "project": v.controlPath.project,
			}
		} else {
			out["control_path_error"] = v.controlPathErr
		}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
