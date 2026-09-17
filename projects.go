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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	defer r.mu.Unlock()
	r.byName = make(map[string]*ProjectEntry, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		r.byName[e.Name] = e
	}
	slog.Info("compose registry loaded", "count", len(r.byName), "path", r.path)
	return nil
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
func (r *composeRegistry) enrichFromLive(live []ComposeProject) {
	var added int
	r.mu.Lock()
	for _, p := range live {
		if p.Name == "" || p.WorkingDir == "" {
			continue
		}
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
// capabilities (projectCapabilities). A registered entry's working dir wins over
// the live label: it is the path an op would actually load. selfProject is the
// agent's own compose project ("" when unknown).
func (r *composeRegistry) mergeKnown(live []ComposeProject, selfProject string) []ComposeProject {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]int, len(live))
	for i, p := range live {
		seen[p.Name] = i
	}
	for i := range live {
		wd := live[i].WorkingDir
		if e, ok := r.byName[live[i].Name]; ok {
			wd = e.WorkingDir
		}
		live[i].stampCapability(projectCapabilities(live[i].Name, wd, r.composeRoot, selfProject))
	}
	for name, e := range r.byName {
		if _, ok := seen[name]; ok {
			continue
		}
		p := ComposeProject{Name: name, WorkingDir: e.WorkingDir}
		p.stampCapability(projectCapabilities(name, e.WorkingDir, r.composeRoot, selfProject))
		live = append(live, p)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	return live
}

func (p *ComposeProject) stampCapability(c projectCapability) {
	p.Operable, p.Managed, p.OpsBlocked = c.Operable, c.Editable, c.Blocked
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
