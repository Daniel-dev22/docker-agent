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
	// Owner names the tool that RENDERS this stack's files, when it is not the
	// agent. Empty means the agent wrote them (register-with-files) or nobody
	// claimed them, and they are editable here. Non-empty ("ansible") means the
	// files are generated somewhere else and will be overwritten by their owner:
	// the agent still OPERATES the stack — up/pull/recreate/update, including the
	// image-tag write into its .env — but never rewrites the files wholesale and
	// never serves them as a copy source. Declared on register; see
	// registerProjectBody.Owner for why an absent owner preserves it.
	Owner string `json:"owner,omitempty"`
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

	// unmarked holds the entries whose owner the index knows and whose DIRECTORY
	// does not record (ownermark.go), recomputed on the same edges as shared.
	// Derived here rather than in the readiness handler so a health probe does no
	// file I/O — the same reason shared is cached.
	unmarked atomic.Pointer[[]string]
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
	r.reconcileOwnerMarks() // before the listener binds: no request may see an entry whose owner is stale
	r.refreshShared()       // before the listener binds, so readiness never reports "none" for "not yet checked"
	return nil
}

// reconcileOwnerMarks makes the index agree with the directories it indexes, with
// the DIRECTORY winning — it is the durable record, and the index is the copy
// that can be lost (ownermark.go).
//
// It also MIGRATES: an entry whose owner the index knows and the directory does
// not gets its mark written here, so the very first restart after this ships
// protects every stack the fleet already converged, with no role run and no
// operator step. An entry that has no mark and no owner is left alone — that is
// the overwhelmingly common case and it costs one failed open.
func (r *composeRegistry) reconcileOwnerMarks() {
	var changed bool
	for _, e := range r.list() {
		if !underComposeRoot(e.WorkingDir, r.composeRoot) {
			continue // ownership is only durable, and only meaningful, under the root
		}
		mark, err := readOwnerMark(e.WorkingDir, r.composeRoot)
		if err != nil {
			slog.Warn("owner mark unreadable — treating the stack as externally rendered",
				"project", e.Name, "working_dir", e.WorkingDir, "error", err)
		}
		switch {
		case mark == "" && e.Owner != "":
			if werr := writeOwnerMark(e.WorkingDir, r.composeRoot, e.Owner); werr != nil {
				// The index still protects it for this process's lifetime; what is lost
				// is recovery from a registry loss, which is what readiness reports.
				slog.Warn("could not record the owner in its stack directory",
					"project", e.Name, "owner", e.Owner, "working_dir", e.WorkingDir, "error", werr)
			} else {
				slog.Info("recorded the owner in its stack directory",
					"project", e.Name, "owner", e.Owner, "working_dir", e.WorkingDir)
			}
		case mark != "" && mark != e.Owner:
			slog.Info("owner taken from the stack directory, which outranks the index",
				"project", e.Name, "index_owner", e.Owner, "directory_owner", mark, "working_dir", e.WorkingDir)
			r.setOwner(e.Name, mark)
			changed = true
		}
	}
	if changed {
		if err := r.persist(); err != nil {
			slog.Warn("persist reconciled owners failed", "error", err)
		}
	}
}

// setOwner updates the cached owner of a registered entry. The directory is the
// source; this keeps the index and the wire value in step with it.
func (r *composeRegistry) setOwner(name, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byName[name]; ok {
		e.Owner = owner
	}
}

// ownerOfDir answers "who do the files in dir belong to right now?" from the
// directory itself, which outlives the index. "" means nobody claims them.
//
// Every write guard asks this rather than the index: the index is what a lost
// projects.json, or a deregister, takes away.
func (r *composeRegistry) ownerOfDir(dir string) string {
	owner, _ := readOwnerMark(dir, r.composeRoot) // fails safe to unknownOwner
	return owner
}

// unmarkedOwners returns the entries whose owner the index holds but whose
// directory does not record — ownership that would NOT survive losing
// projects.json. Reads the cache refreshed by refreshUnmarked, so readiness
// answers without touching the filesystem.
func (r *composeRegistry) unmarkedOwners() []string {
	if r == nil {
		return []string{} // readiness must answer even before a registry exists
	}
	if p := r.unmarked.Load(); p != nil && len(*p) > 0 {
		return *p
	}
	return []string{}
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
	r.refreshUnmarked()
}

// refreshUnmarked recomputes which owned entries have no record in their own
// directory. Logged on CHANGE only: it is recomputed on every index edit, and a
// standing problem that logged each time would bury the edit that caused it.
func (r *composeRegistry) refreshUnmarked() {
	var unmarked []string
	for _, e := range r.list() {
		if e.Owner == "" || !underComposeRoot(e.WorkingDir, r.composeRoot) {
			continue
		}
		if mark, _ := readOwnerMark(e.WorkingDir, r.composeRoot); mark == "" {
			unmarked = append(unmarked, e.Name)
		}
	}
	sort.Strings(unmarked)
	if prev := r.unmarked.Swap(&unmarked); (prev == nil && len(unmarked) > 0) || (prev != nil && !slices.Equal(*prev, unmarked)) {
		if len(unmarked) > 0 {
			slog.Warn("compose registry: an owner is recorded in the index but not in its stack directory — "+
				"losing projects.json would make these editable again", "projects", unmarked)
		} else {
			slog.Info("compose registry: every recorded owner is durable in its own stack directory")
		}
	}
}

// register adds or replaces a durable project entry and rewrites projects.json.
func (r *composeRegistry) register(e ProjectEntry) error {
	if e.Name == "" {
		return errors.New("project name is required")
	}
	// The directory records the owner BEFORE the index does, for every caller —
	// the invariant is "the index never holds an owner its directory does not",
	// and a rule enforced in one handler is a rule the next caller forgets. A
	// failure here fails the register: an index-only owner is the state this
	// mechanism exists to prevent. The reverse order fails safe — an owner
	// recorded with no index entry is re-adopted owned.
	//
	// An empty Owner REMOVES the mark, so taking the files back is the same one
	// call it always was.
	if err := writeOwnerMark(e.WorkingDir, r.composeRoot, e.Owner); err != nil {
		return err
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
			return projectEntryFromLive(p, r.composeRoot), true
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
	// Build the entries — which READS each directory's owner mark — before taking
	// the lock. The registry lock is on the fleet snapshot's path; file I/O does
	// not belong under it.
	ordered := slices.Sorted(maps.Keys(candidates))
	adopting := make([]ProjectEntry, 0, len(ordered))
	for _, dir := range ordered {
		e := projectEntryFromLive(candidates[dir], r.composeRoot)
		e.RegisteredAt = time.Now().UTC()
		// Adopted marks an entry not yet persisted; these are about to be.
		e.Adopted = false
		adopting = append(adopting, e)
	}
	var added int
	r.mu.Lock()
	for i := range adopting {
		e := adopting[i]
		if _, ok := r.byName[e.Name]; ok {
			continue
		}
		if e.Owner != "" {
			slog.Info("adopted a running compose project its directory says is externally rendered",
				"project", e.Name, "owner", e.Owner, "working_dir", e.WorkingDir)
		}
		r.byName[e.Name] = &e
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
		// A registered entry decides: its working dir is what an op loads, and its
		// owner is who renders the files there. An unregistered running project has
		// neither — the label's dir, and no owner.
		row := ProjectEntry{Name: live[i].Name, WorkingDir: live[i].WorkingDir}
		if e, ok := r.byName[live[i].Name]; ok {
			row = *e
			live[i].WorkingDir = e.WorkingDir
		} else {
			// No index entry to carry an owner, so the directory answers. This reads a
			// file under the registry's read lock, which is only tolerable because the
			// set is EMPTY in steady state — boot adoption registers every running
			// project — and non-empty exactly when the index has been lost or an entry
			// deregistered, which is when a wrong answer here offers Edit on Ansible's
			// files. The error is deliberately dropped: readOwnerMark already fails
			// safe, and this runs once per snapshot.
			row.Owner, _ = readOwnerMark(row.WorkingDir, r.composeRoot)
		}
		live[i].stampCapability(projectCapabilities(row, r.composeRoot, v))
	}
	for name, e := range r.byName {
		if _, ok := seen[name]; ok {
			continue
		}
		p := ComposeProject{Name: name, WorkingDir: e.WorkingDir}
		p.stampCapability(projectCapabilities(*e, r.composeRoot, v))
		live = append(live, p)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	return live
}

func (p *ComposeProject) stampCapability(c projectCapability) {
	p.AllowedOps, p.Operable, p.Managed, p.OpsBlocked = c.Allowed, c.operable(), c.Editable, c.Blocked
	p.ServiceOps = c.serviceOps()
	p.Owner = c.Owner
}

// projectEntryFromLive builds an entry from a label-derived ComposeProject. The
// config_files label is a comma-separated list of absolute paths.
//
// A container label never carries an owner, so the OWNER COMES FROM THE
// DIRECTORY (ownermark.go). Without that, every path that derives an entry from
// live containers — boot adoption, and an op resolved against a name the index
// does not hold — produces an unowned, editable entry for files Ansible renders.
// That is exactly what a lost projects.json did.
func projectEntryFromLive(p ComposeProject, composeRoot string) ProjectEntry {
	var files []string
	if p.ConfigFiles != "" {
		for _, f := range strings.Split(p.ConfigFiles, ",") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
	}
	owner, _ := readOwnerMark(p.WorkingDir, composeRoot) // fails safe; logged by the callers that run once
	return ProjectEntry{
		Name:         p.Name,
		WorkingDir:   p.WorkingDir,
		ComposeFiles: files,
		Owner:        owner,
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
