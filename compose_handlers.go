package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// Compose project HTTP surface. Project ops are long async jobs (202 + job id);
// their logs stream over /ws/jobs/:id/logs. Project CRUD + copy/bundle are
// synchronous registry/file operations.
// ---------------------------------------------------------------------------

// composeOpBody is the body for POST /v1/projects/:name/op.
type composeOpBody struct {
	Op         string `json:"op"`                // up|down|pull|restart|recreate|update
	Timeout    *int   `json:"timeout,omitempty"` // restart: container stop grace seconds
	TriggerKey string `json:"trigger_key,omitempty"`
	// Stack-update (op=update) options. OverrideImage deploys an exact image;
	// OverrideService targets a specific service for the override.
	OverrideImage   string `json:"override_image,omitempty"`
	OverrideService string `json:"override_service,omitempty"`
	// One-shot health/swap budget in seconds for this job only (see JobRequest).
	HealthTimeoutS int `json:"health_timeout_s,omitempty"`
	SwapTimeoutS   int `json:"swap_timeout_s,omitempty"`
}

func (a *app) handleComposeOp(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project name is required"})
		return
	}
	var body composeOpBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !projectOps[body.Op] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "op must be one of up|down|pull|restart|recreate|update"})
		return
	}
	// One fresh list serves both the live-project fallback and the self view, so the
	// entry and the protection were decided from the same moment.
	summaries, view, proceed := a.selfForMutation(c)
	if !proceed {
		return
	}
	entry, ok := a.projects.get(name)
	if !ok && summaries != nil {
		entry, ok = a.projects.resolve(name, groupComposeProjects(summaries))
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project: " + name})
		return
	}
	// Refuse what can never run BEFORE anything that could create a job: a refusal
	// is a decision, and a failed job row would read as an attempt that broke. It
	// also outranks a transiently unavailable backend below — retrying cannot fix it.
	if refuseProjectOp(c, entry, a.cfg.ComposeRoot, a.capabilityOf(entry, view), body.Op) {
		return
	}
	if a.compose == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "compose backend unavailable on this host"})
		return
	}
	// Bound the one-shot budgets HERE, not only at the router. The router validates
	// the strategy row, but it is not a gate on this path: the per-node proxy replays
	// method and body verbatim, and the agent is reachable in-process on the docker
	// network with no bearer or mTLS. An unbounded value overflows time.Duration into
	// a negative deadline, which the budget clamp cannot see. Refusing is better than
	// silently ignoring — the caller asked for something and deserves to be told it
	// was not honoured.
	if !validBudgetSeconds(body.HealthTimeoutS) || !validBudgetSeconds(body.SwapTimeoutS) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("health_timeout_s and swap_timeout_s must be between 0 and %d seconds (0 = use the project/node default)",
				int(maxBudget/time.Second))})
		return
	}
	// An override image is written into the project's compose file or .env and then
	// pulled. Same reasoning as the budget check above — this endpoint takes a
	// replayed body with no bearer or mTLS of its own — but the consequence is
	// worse than a bad deadline: an unchecked value with a newline appends lines to
	// the stack's .env, and compose honours every one of them on the next `up`.
	if body.OverrideImage != "" && !validImageRef(body.OverrideImage) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "override_image must be a plain image reference (registry/name:tag[@sha256:…]); " +
				"whitespace, control characters and shell or interpolation metacharacters are refused"})
		return
	}
	j := a.reg.start(context.Background(), JobRequest{
		Operation:       body.Op,
		Project:         name,
		Timeout:         body.Timeout,
		OverrideImage:   body.OverrideImage,
		OverrideService: body.OverrideService,
		HealthTimeoutS:  body.HealthTimeoutS,
		SwapTimeoutS:    body.SwapTimeoutS,
		TriggerKey:      orDefault(body.TriggerKey, "ui"),
	})
	c.JSON(http.StatusAccepted, gin.H{"job_id": j.ID, "state": j.snapshot().State})
}

// validBudgetSeconds reports whether a request-supplied budget is one the agent
// will honour. 0 stays legal — it is how a caller says "use the configured
// default" — so this rejects only negatives and anything past maxBudget.
func validBudgetSeconds(v int) bool {
	return v == 0 || (v > 0 && time.Duration(v)*time.Second > 0 && time.Duration(v)*time.Second <= maxBudget)
}

// capabilityOf is projectCapabilities for an entry the handlers resolved.
func (a *app) capabilityOf(e ProjectEntry, v *selfView) projectCapability {
	return projectCapabilities(e.Name, e.WorkingDir, a.cfg.ComposeRoot, v)
}

// --- project registry CRUD ---

// projectListEntry is one GET /v1/projects row: the durable entry plus the same
// capability fields the fleet snapshot carries, so a consumer that works from this
// list (system_monitor's network remediation) can ask before it acts.
type projectListEntry struct {
	ProjectEntry
	AllowedOps []string `json:"allowed_ops"`
	Operable   bool     `json:"operable"`
	Managed    bool     `json:"managed"`
	OpsBlocked string   `json:"ops_blocked,omitempty"`
}

// viewForRead is the self view a READ reports capability from. It takes a fresh
// list; if that fails, the last published view is used only when it can still
// state what is protected — it found the agent's own container (whose ID and labels
// cannot change for this process's lifetime), or, with no mountinfo ID, it exists
// at all. Otherwise it writes a 503 and returns ok=false: stating "operable" blind
// is the over-claim these reads exist to prevent. Mutations never fall back — see
// selfForMutation.
func (a *app) viewForRead(c *gin.Context) (*selfView, bool) {
	_, view, err := a.observeNow(c.Request.Context())
	if err == nil {
		return view, true
	}
	view = a.self.current()
	stale := view == nil && a.self.protects()
	if view != nil && a.self.known() && !view.found {
		stale = true
	}
	if stale {
		refuseSelfUnavailable(c, err, nil)
		return nil, false
	}
	return view, true
}

// handleListProjects reports each entry's capability (viewForRead).
func (a *app) handleListProjects(c *gin.Context) {
	view, ok := a.viewForRead(c)
	if !ok {
		return
	}
	entries := a.projects.list()
	out := make([]projectListEntry, 0, len(entries))
	for _, e := range entries {
		capa := a.capabilityOf(e, view)
		out = append(out, projectListEntry{
			ProjectEntry: e, AllowedOps: capa.Allowed, Operable: capa.operable(),
			Managed: capa.Editable, OpsBlocked: capa.Blocked,
		})
	}
	c.JSON(http.StatusOK, gin.H{"projects": out})
}

// registerProjectBody registers a project. Two modes: (1) point at an existing
// on-host dir (working_dir + compose_files), or (2) create from inline content
// (files: relpath→content written under <ComposeRoot>/<name>/). Mode 2 is how a
// cross-host copy lands a stack on a target node (frontend GETs the source
// bundle, POSTs it here). deploy=true runs `up` immediately after registering.
type registerProjectBody struct {
	Name         string            `json:"name"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	ComposeFiles []string          `json:"compose_files,omitempty"`
	Profiles     []string          `json:"profiles,omitempty"`
	EnvFiles     []string          `json:"env_files,omitempty"`
	Files        map[string]string `json:"files,omitempty"`
	Deploy       bool              `json:"deploy,omitempty"`
}

func (a *app) handleRegisterProject(c *gin.Context) {
	var body registerProjectBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Input first: a malformed request is a 400 whatever the capability would be.
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if !validProjectName(body.Name) {
		refuseInvalidName(c, body.Name)
		return
	}
	if len(body.Files) == 0 && body.WorkingDir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "working_dir or files is required"})
		return
	}
	entry := ProjectEntry{
		Name:         body.Name,
		WorkingDir:   body.WorkingDir,
		ComposeFiles: body.ComposeFiles,
		Profiles:     body.Profiles,
		EnvFiles:     body.EnvFiles,
	}
	if len(body.Files) > 0 {
		// Inline files always land under ComposeRoot; decide on that directory.
		entry.WorkingDir = filepath.Join(a.cfg.ComposeRoot, body.Name)
	}
	if !filepath.IsAbs(entry.WorkingDir) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "working_dir must be absolute", "working_dir": entry.WorkingDir})
		return
	}
	entry.WorkingDir = filepath.Clean(entry.WorkingDir)
	if err := validateDeclaredPaths(entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "project_path_outside_working_dir", "working_dir": entry.WorkingDir})
		return
	}
	summaries, view, proceed := a.selfForMutation(c)
	if !proceed {
		return
	}
	// Every refusal happens BEFORE writeProjectFiles, which overwrites: a refused
	// request must not have rewritten anything — least of all the agent's own
	// compose file.
	capa := a.capabilityOf(entry, view)
	if capa.Blocked == blockedSelf {
		refuseSelfProject(c, entry)
		return
	}
	if body.Deploy {
		if from, moved := a.relocation(entry, summaries); moved {
			refuseRelocationDeploy(c, entry, from)
			return
		}
	}
	if len(body.Files) == 0 && capa.Editable {
		// A path-only register under ComposeRoot claims managed:true. Make that claim
		// true now, not an empty bundle discovered later.
		if err := composeFilesPresent(entry); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "working_dir": entry.WorkingDir})
			return
		}
	}
	if body.Deploy && refuseProjectOp(c, entry, a.cfg.ComposeRoot, capa, opComposeUp) {
		return
	}
	if len(body.Files) > 0 {
		dir, written, err := writeProjectFiles(a.cfg.ComposeRoot, body.Name, body.Files)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		entry.WorkingDir = dir
		if len(entry.ComposeFiles) == 0 {
			entry.ComposeFiles = composeFileNames(written)
		}
	}
	if err := a.projects.register(entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{"registered": entry.Name}
	if body.Deploy && a.compose != nil {
		j := a.reg.start(context.Background(), JobRequest{
			Operation: opComposeUp, Project: entry.Name, TriggerKey: "register",
		})
		resp["job_id"] = j.ID
	}
	c.JSON(http.StatusOK, resp)
}

// relocation reports whether registering entry moves an existing project: the name
// is already registered, or already running, with a different working dir. from is
// that existing dir.
func (a *app) relocation(entry ProjectEntry, summaries []container.Summary) (from string, moved bool) {
	if e, ok := a.projects.get(entry.Name); ok {
		return e.WorkingDir, filepath.Clean(e.WorkingDir) != entry.WorkingDir
	}
	for _, p := range groupComposeProjects(summaries) {
		if p.Name == entry.Name {
			return p.WorkingDir, filepath.Clean(p.WorkingDir) != entry.WorkingDir
		}
	}
	return "", false
}

// composeDefaultFiles are the names compose auto-discovers in a working dir, in its
// own preference order.
var composeDefaultFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// composeFilesPresent checks a path-only entry's compose files can be read: each
// declared file, or — with none declared — one compose auto-discovery would find.
func composeFilesPresent(e ProjectEntry) error {
	readable := func(p string) bool {
		f, err := os.Open(p)
		if err != nil {
			return false
		}
		defer f.Close()
		st, err := f.Stat()
		return err == nil && st.Mode().IsRegular()
	}
	if files := e.absComposeFiles(); len(files) > 0 {
		for _, f := range files {
			if !readable(f) {
				return fmt.Errorf("compose file %s for %s is missing or unreadable", f, e.Name)
			}
		}
		return nil
	}
	for _, name := range composeDefaultFiles {
		if readable(filepath.Join(e.WorkingDir, name)) {
			return nil
		}
	}
	return fmt.Errorf("no compose file (%s) in %s for %s", strings.Join(composeDefaultFiles, ", "), e.WorkingDir, e.Name)
}

func (a *app) handleDeregisterProject(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project name is required"})
		return
	}
	if err := a.projects.deregister(name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deregistered": name})
}

// --- copy / duplicate ---

// bundleFile is one file's name (basename) + content in a project bundle.
type bundleFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// projectBundle is the portable form of a project: its compose + env file
// contents. Used by the frontend to copy a stack to another host (GET here,
// POST to the target node's /v1/projects with Files).
type projectBundle struct {
	Name         string       `json:"name"`
	WorkingDir   string       `json:"working_dir"`
	ComposeFiles []bundleFile `json:"compose_files"`
	EnvFiles     []bundleFile `json:"env_files"`
}

// handleProjectBundle returns a project's file contents only when the project is
// editable here. Any other known project gets the same typed, empty bundle an
// unreadable one always did — the editor renders its "not editable, files live at
// <working_dir>" notice from exactly that shape — and no content.
func (a *app) handleProjectBundle(c *gin.Context) {
	name := c.Param("name")
	entry, ok := a.projects.get(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project: " + name})
		return
	}
	view, ok := a.viewForRead(c)
	if !ok {
		return
	}
	if !a.capabilityOf(entry, view).Editable {
		c.JSON(http.StatusOK, projectBundle{Name: entry.Name, WorkingDir: entry.WorkingDir,
			ComposeFiles: []bundleFile{}, EnvFiles: []bundleFile{}})
		return
	}
	bundle, err := a.readBundle(entry)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, bundle)
}

// copyProjectBody duplicates a project on THIS host under a new name.
type copyProjectBody struct {
	NewName string `json:"new_name"`
	Deploy  bool   `json:"deploy,omitempty"`
}

func (a *app) handleCopyProject(c *gin.Context) {
	name := c.Param("name")
	var body copyProjectBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.NewName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new_name is required"})
		return
	}
	if !validProjectName(body.NewName) {
		refuseInvalidName(c, body.NewName)
		return
	}
	if _, exists := a.projects.get(body.NewName); exists {
		c.JSON(http.StatusConflict, gin.H{"error": "project already exists: " + body.NewName})
		return
	}
	src, ok := a.projects.get(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project: " + name})
		return
	}
	_, view, proceed := a.selfForMutation(c)
	if !proceed {
		return
	}
	// A copy reads the source's files, so the source must be editable; and the copy
	// must not take one of the agent's own project names, whose `up` would recreate
	// — or remove as an orphan — the agent itself. Both refusals precede any write.
	if refuseProjectEdit(c, src, a.cfg.ComposeRoot, a.capabilityOf(src, view)) {
		return
	}
	dst := ProjectEntry{Name: body.NewName, WorkingDir: filepath.Join(a.cfg.ComposeRoot, body.NewName)}
	dstCapa := a.capabilityOf(dst, view)
	if dstCapa.Blocked == blockedSelf {
		refuseSelfProject(c, dst)
		return
	}
	if body.Deploy && refuseProjectOp(c, dst, a.cfg.ComposeRoot, dstCapa, opComposeUp) {
		return
	}
	bundle, err := a.readBundle(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	files := map[string]string{}
	for _, f := range bundle.ComposeFiles {
		files[f.Name] = f.Content
	}
	for _, f := range bundle.EnvFiles {
		files[f.Name] = f.Content
	}
	dir, written, err := writeProjectFiles(a.cfg.ComposeRoot, body.NewName, files)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	entry := ProjectEntry{Name: body.NewName, WorkingDir: dir, ComposeFiles: composeFileNames(written)}
	if err := a.projects.register(entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp := gin.H{"copied": body.NewName, "working_dir": dir}
	if body.Deploy && a.compose != nil {
		j := a.reg.start(context.Background(), JobRequest{
			Operation: opComposeUp, Project: body.NewName, TriggerKey: "copy",
		})
		resp["job_id"] = j.ID
	}
	c.JSON(http.StatusOK, resp)
}

// readBundle reads a project's compose + env file contents off disk. A file that
// is unreadable, or that resolves (symlinks followed) outside ComposeRoot, is
// SKIPPED and logged, never read: the bundle returns file content over the API, so
// a registered path must not be a way to read an arbitrary file the container can
// see. Callers gate on the project being editable first.
func (a *app) readBundle(e ProjectEntry) (projectBundle, error) {
	b := projectBundle{Name: e.Name, WorkingDir: e.WorkingDir, ComposeFiles: []bundleFile{}, EnvFiles: []bundleFile{}}
	read := func(p string) (string, bool) {
		ok, err := confinedUnder(p, a.cfg.ComposeRoot)
		if err != nil || !ok {
			if err == nil || !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("compose bundle: skipping file outside the agent's compose root or unreadable",
					"project", e.Name, "file", p, "error", err)
			}
			return "", false
		}
		data, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("compose bundle: skipping unreadable file", "project", e.Name, "file", p, "error", err)
			return "", false
		}
		return string(data), true
	}
	for _, p := range e.absComposeFiles() {
		if content, ok := read(p); ok {
			b.ComposeFiles = append(b.ComposeFiles, bundleFile{Name: filepath.Base(p), Content: content})
		}
	}
	seen := map[string]struct{}{}
	for _, p := range envFilesToLoad(e) {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		if content, ok := read(p); ok {
			b.EnvFiles = append(b.EnvFiles, bundleFile{Name: filepath.Base(p), Content: content})
		}
	}
	return b, nil
}

// writeProjectFiles writes a bundle's files under <root>/<name>/, rejecting any
// path that escapes the project dir. Returns the created dir + the relative
// names written.
func writeProjectFiles(root, name string, files map[string]string) (string, []string, error) {
	if root == "" {
		return "", nil, fmt.Errorf("compose root not configured")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", nil, fmt.Errorf("invalid project name %q", name)
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	var written []string
	for rel, content := range files {
		clean := strings.TrimPrefix(filepath.Clean("/"+rel), "/")
		if clean == "" || clean == "." || strings.HasPrefix(clean, "..") {
			return "", nil, fmt.Errorf("invalid file path %q", rel)
		}
		full := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", nil, err
		}
		written = append(written, clean)
	}
	return dir, written, nil
}

// composeFileNames filters a written-file list down to the compose YAML files,
// so a copied/registered project's ComposeFiles excludes .env and other sidecars.
func composeFileNames(written []string) []string {
	var out []string
	for _, f := range written {
		base := strings.ToLower(filepath.Base(f))
		if (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")) &&
			(strings.Contains(base, "compose") || strings.Contains(base, "docker-compose")) {
			out = append(out, f)
		}
	}
	return out
}
