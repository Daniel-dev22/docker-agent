package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	if a.compose == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "compose backend unavailable on this host"})
		return
	}
	if !a.projectKnown(c.Request.Context(), name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project: " + name})
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

// projectKnown reports whether the name is in the durable registry or among the
// currently-running compose projects (so an ad-hoc running stack is operable).
func (a *app) projectKnown(ctx context.Context, name string) bool {
	if _, ok := a.projects.get(name); ok {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, live, err := a.docker.snapshot(cctx); err == nil {
		for _, p := range live {
			if p.Name == name {
				return true
			}
		}
	}
	return false
}

// --- project registry CRUD ---

func (a *app) handleListProjects(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"projects": a.projects.list()})
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
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
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
	if entry.WorkingDir == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "working_dir or files is required"})
		return
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

func (a *app) handleProjectBundle(c *gin.Context) {
	name := c.Param("name")
	entry, ok := a.projects.get(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project: " + name})
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
	if _, exists := a.projects.get(body.NewName); exists {
		c.JSON(http.StatusConflict, gin.H{"error": "project already exists: " + body.NewName})
		return
	}
	src, ok := a.projects.get(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown project: " + name})
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

// readBundle reads a project's compose + env file contents off disk. Unreadable
// compose files are SKIPPED (logged), not fatal: an externally-provisioned stack
// whose working dir is outside the agent's bind-mounted
// ComposeRoot returns an empty/partial bundle + its working_dir, so the UI shows
// its graceful "not editable, files live at <working_dir>" notice instead of a
// raw 500. (Such stacks are also flagged Managed=false up front — see mergeKnown.)
func (a *app) readBundle(e ProjectEntry) (projectBundle, error) {
	b := projectBundle{Name: e.Name, WorkingDir: e.WorkingDir}
	for _, p := range e.absComposeFiles() {
		data, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("compose bundle: skipping unreadable file (likely outside the agent's ComposeRoot mount)",
				"project", e.Name, "file", p, "error", err)
			continue
		}
		b.ComposeFiles = append(b.ComposeFiles, bundleFile{Name: filepath.Base(p), Content: string(data)})
	}
	// Include any env files plus a conventional .env in the working dir.
	envPaths := map[string]struct{}{}
	for _, ef := range e.EnvFiles {
		if !filepath.IsAbs(ef) && e.WorkingDir != "" {
			ef = filepath.Join(e.WorkingDir, ef)
		}
		envPaths[ef] = struct{}{}
	}
	if e.WorkingDir != "" {
		envPaths[filepath.Join(e.WorkingDir, ".env")] = struct{}{}
	}
	for p := range envPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue // env files are optional
		}
		b.EnvFiles = append(b.EnvFiles, bundleFile{Name: filepath.Base(p), Content: string(data)})
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
