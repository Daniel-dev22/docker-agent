package main

// Compose-project registry.
//
// The durable projects.json (under the bind-mounted ComposeRoot) is the SOLE
// source of truth for which projects exist when stopped — NEVER a filesystem
// crawl. It is seeded by explicit register calls. Currently-RUNNING projects are
// additionally auto-adopted from their `com.docker.compose.project*` labels on
// boot, so an operator never has to register a project that's already up.
//
// projects.json carries NO secrets — only names, working dirs, compose-file
// paths, profiles, env files. Compose data (the files themselves) lives on host
// under ComposeRoot/<stack>/ (data plane); this registry is just the index.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProjectEntry is one managed compose project. ComposeFiles are stored as given
// (absolute, or relative to WorkingDir); absComposeFiles resolves them. An empty
// ComposeFiles lets compose-go auto-discover the file in WorkingDir.
type ProjectEntry struct {
	Name         string    `json:"name"`
	WorkingDir   string    `json:"working_dir"`
	ComposeFiles []string  `json:"compose_files,omitempty"`
	Profiles     []string  `json:"profiles,omitempty"`
	EnvFiles     []string  `json:"env_files,omitempty"`
	RegisteredAt time.Time `json:"registered_at,omitempty"`
	// Adopted is true when the entry was derived from live container labels and
	// is not (yet) persisted to projects.json. Not serialized.
	Adopted bool `json:"-"`
}

// absComposeFiles resolves each compose file against WorkingDir (absolute paths
// pass through). Empty → nil, so compose-go auto-discovers in WorkingDir.
func (e ProjectEntry) absComposeFiles() []string {
	if len(e.ComposeFiles) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.ComposeFiles))
	for _, f := range e.ComposeFiles {
		if filepath.IsAbs(f) || e.WorkingDir == "" {
			out = append(out, f)
		} else {
			out = append(out, filepath.Join(e.WorkingDir, f))
		}
	}
	return out
}

// composeRegistry owns the durable project index. The byName map holds entries
// loaded from / persisted to projects.json. Live (label-adopted) projects are
// merged in on read so the dashboard sees both running and stopped-but-known
// projects.
type composeRegistry struct {
	path        string // <ComposeRoot>/projects.json
	composeRoot string // the bind-mounted data dir; a stack under it is agent-owned/editable

	mu     sync.RWMutex
	byName map[string]*ProjectEntry

	persistMu sync.Mutex // serializes the write-tmp/rename pair

	// shared holds the working directories more than one entry names
	// (canonical dir → names), recomputed on every change to the index. The
	// register handler refuses to create one, and auto-adoption skips one; what
	// remains predates that rule and is reported in readiness.
	shared atomic.Pointer[map[string][]string]
}

func newComposeRegistry(path, composeRoot string) *composeRegistry {
	return &composeRegistry{path: path, composeRoot: composeRoot, byName: map[string]*ProjectEntry{}}
}

// underComposeRoot reports whether workingDir lives inside root. The agent's own
// register/copy is the ONLY writer under ComposeRoot (writeProjectFiles), so a
// stack whose working dir is under it is one the agent created and can read/write
// (editable in place). Externally-provisioned stacks live elsewhere on the host
// — outside the agent's mount — and are NOT editable here.
// Structural (no I/O) and self-correcting: it needs no migration of existing
// projects.json entries and never depends on a persisted provenance flag.
func underComposeRoot(workingDir, root string) bool {
	if workingDir == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, workingDir)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))
}

// load reads projects.json. Missing file is OK (empty registry); a malformed
// file is fatal — surface corruption rather than silently dropping the index.
func (r *composeRegistry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", r.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var entries []*ProjectEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse %s: %w", r.path, err)
	}
	r.mu.Lock()
	r.byName = make(map[string]*ProjectEntry, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		r.byName[e.Name] = e
	}
	count := len(r.byName)
	r.mu.Unlock()
	slog.Info("compose registry loaded", "count", count, "path", r.path)
	r.refreshShared() // before the listener binds, so readiness never reports "none" for "not yet checked"
	return nil
}

// canonicalDir is dir with symlinks resolved as far as the path exists, so two
// spellings of one directory — or a link to it — compare equal. The part that
// does not exist yet (a project directory about to be created) stays lexical.
func canonicalDir(dir string) string {
	if dir == "" {
		return ""
	}
	p, rest := filepath.Clean(dir), ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// workingDirOwner returns the registered project, other than name, whose working
// directory is dir.
func (r *composeRegistry) workingDirOwner(dir, name string) (string, bool) {
	want := canonicalDir(dir)
	for _, e := range r.list() {
		if e.Name != name && e.WorkingDir != "" && canonicalDir(e.WorkingDir) == want {
			return e.Name, true
		}
	}
	return "", false
}

// sharedWorkingDirs returns the working directories more than one entry names.
func (r *composeRegistry) sharedWorkingDirs() map[string][]string {
	if r == nil {
		return map[string][]string{} // readiness must answer even before a registry exists
	}
	if p := r.shared.Load(); p != nil {
		return *p
	}
	return map[string][]string{}
}

func (r *composeRegistry) refreshShared() {
	byDir := map[string][]string{}
	for _, e := range r.list() {
		if e.WorkingDir != "" {
			d := canonicalDir(e.WorkingDir)
			byDir[d] = append(byDir[d], e.Name)
		}
	}
	shared := map[string][]string{}
	for d, names := range byDir {
		if len(names) > 1 {
			shared[d] = names
		}
	}
	if prev := r.shared.Swap(&shared); (prev == nil && len(shared) > 0) || (prev != nil && !reflect.DeepEqual(*prev, shared)) {
		if len(shared) > 0 {
			slog.Warn("compose registry: projects share a working directory — changes to one are serialised with the other, "+
				"but deregister all but one", "shared", shared)
		} else {
			slog.Info("compose registry: no projects share a working directory")
		}
	}
}

// register adds or replaces a durable project entry and rewrites projects.json.
func (r *composeRegistry) register(e ProjectEntry) error {
	if e.Name == "" {
		return errors.New("project name is required")
	}
	r.mu.Lock()
	if e.RegisteredAt.IsZero() {
		e.RegisteredAt = time.Now().UTC()
	}
	e.Adopted = false
	ec := e
	r.byName[e.Name] = &ec
	r.mu.Unlock()
	defer r.refreshShared()
	return r.persist()
}

// deregister drops a project from the durable index. It does NOT touch the
// project's on-host compose files or its containers.
func (r *composeRegistry) deregister(name string) error {
	r.mu.Lock()
	_, ok := r.byName[name]
	delete(r.byName, name)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	defer r.refreshShared()
	return r.persist()
}

// get returns the entry for a project name, if known (durable index only).
func (r *composeRegistry) get(name string) (ProjectEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byName[name]
	if !ok {
		return ProjectEntry{}, false
	}
	return *e, true
}

// list returns durable entries sorted by name.
func (r *composeRegistry) list() []ProjectEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProjectEntry, 0, len(r.byName))
	for _, e := range r.byName {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolve returns the entry to run an op against. A durably-registered entry
// wins; otherwise it falls back to a live project derived from the current
// container labels (so ad-hoc / not-yet-registered running projects are still
// operable). Returns false when neither knows the name.
func (r *composeRegistry) resolve(name string, live []ComposeProject) (ProjectEntry, bool) {
	if e, ok := r.get(name); ok {
		return e, true
	}
	for _, p := range live {
		if p.Name == name {
			return projectEntryFromLive(p), true
		}
	}
	return ProjectEntry{}, false
}

// enrichFromLive auto-adopts currently-running projects into the durable index
// (boot + refresh). A project already in projects.json is left untouched (its
// curated paths win); a new running project is persisted so it survives being
// stopped later. Projects with no working_dir label are skipped (nothing to run
// an op against).
//
// A running project whose working directory is already registered under another
// name is not adopted: two entries for one directory would let a change through
// one name bypass a change through the other.
func (r *composeRegistry) enrichFromLive(live []ComposeProject) {
	owners := map[string]string{} // canonical dir → registered name
	for _, e := range r.list() {
		if e.WorkingDir != "" {
			owners[canonicalDir(e.WorkingDir)] = e.Name
		}
	}
	candidates := map[string]ComposeProject{}
	for _, p := range live {
		if p.Name == "" || p.WorkingDir == "" {
			continue
		}
		dir := canonicalDir(p.WorkingDir)
		if owner, ok := owners[dir]; ok && owner != p.Name {
			slog.Warn("not adopting a running compose project: its working directory is registered as another project",
				"project", p.Name, "working_dir", p.WorkingDir, "registered_as", owner)
			continue
		}
		if _, ok := candidates[dir]; ok {
			slog.Warn("not adopting a running compose project: another running project uses its working directory",
				"project", p.Name, "working_dir", p.WorkingDir)
			continue
		}
		candidates[dir] = p
	}
	var added int
	r.mu.Lock()
	for _, dir := range slices.Sorted(maps.Keys(candidates)) {
		p := candidates[dir]
		if _, ok := r.byName[p.Name]; ok {
			continue
		}
		e := projectEntryFromLive(p)
		e.RegisteredAt = time.Now().UTC()
		e.Adopted = false
		r.byName[p.Name] = &e
		added++
	}
	r.mu.Unlock()
	if added > 0 {
		defer r.refreshShared()
		if err := r.persist(); err != nil {
			slog.Warn("persist auto-adopted projects failed", "error", err)
		} else {
			slog.Info("auto-adopted running compose projects", "count", added)
		}
	}
}

// mergeKnown adds stopped-but-registered projects (absent from the live label
// grouping) to the fleet snapshot as zero-container entries, and stamps EVERY
// returned project — registered, registry-only and ad-hoc live — with its
// capabilities (projectCapabilities). A registered entry's working dir replaces
// the live label on the row: it is the path an op loads and the one the
// capability was decided on. v is the current self view (nil = unknown).
func (r *composeRegistry) mergeKnown(live []ComposeProject, v *selfView) []ComposeProject {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]int, len(live))
	for i, p := range live {
		seen[p.Name] = i
	}
	for i := range live {
		if e, ok := r.byName[live[i].Name]; ok {
			live[i].WorkingDir = e.WorkingDir
		}
		live[i].stampCapability(projectCapabilities(live[i].Name, live[i].WorkingDir, r.composeRoot, v))
	}
	for name, e := range r.byName {
		if _, ok := seen[name]; ok {
			continue
		}
		p := ComposeProject{Name: name, WorkingDir: e.WorkingDir}
		p.stampCapability(projectCapabilities(name, e.WorkingDir, r.composeRoot, v))
		live = append(live, p)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	return live
}

func (p *ComposeProject) stampCapability(c projectCapability) {
	p.AllowedOps, p.Operable, p.Managed, p.OpsBlocked = c.Allowed, c.operable(), c.Editable, c.Blocked
	p.ServiceOps = true
}

// projectEntryFromLive builds an entry from a label-derived ComposeProject. The
// config_files label is a comma-separated list of absolute paths.
func projectEntryFromLive(p ComposeProject) ProjectEntry {
	var files []string
	if p.ConfigFiles != "" {
		for _, f := range strings.Split(p.ConfigFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
	}
	return ProjectEntry{
		Name:         p.Name,
		WorkingDir:   p.WorkingDir,
		ComposeFiles: files,
		Adopted:      true,
	}
}

// persist atomically rewrites projects.json (.tmp + rename). persistMu serializes
// the write/rename pair so concurrent registers can't race on the shared .tmp.
func (r *composeRegistry) persist() error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	r.mu.RLock()
	entries := make([]*ProjectEntry, 0, len(r.byName))
	for _, e := range r.byName {
		entries = append(entries, e)
	}
	r.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(r.path), err)
	}
	data, err := json.MarshalIndent(entries, "", "    ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", r.path, err)
	}
	return nil
}
