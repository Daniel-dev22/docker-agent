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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
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
	// Services narrows up, recreate, pull, restart and down to exactly these
	// services (planComposeCall). Absent or empty: the whole project.
	Services []string `json:"services,omitempty"`
}

// maxOpServices bounds a service list: far above any real project, far below a
// request that would make the validation below the expensive part.
const maxOpServices = 100

// composeServiceName is compose's own rule for a service name (compose-spec.json).
var composeServiceName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func (a *app) handleComposeOp(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		refuse(c, http.StatusBadRequest, "invalid_project_name", "project name is required", nil)
		return
	}
	var body composeOpBody
	if err := c.ShouldBindJSON(&body); err != nil {
		refuse(c, http.StatusBadRequest, "invalid_body", "invalid request body: "+echo(err.Error()), nil)
		return
	}
	if !projectOps[body.Op] {
		refuse(c, http.StatusBadRequest, "invalid_op", "op must be one of up|down|pull|restart|recreate|update", nil)
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
		refuse(c, http.StatusNotFound, "unknown_project", "unknown project: "+echo(name), nil)
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
		refuse(c, http.StatusBadRequest, "invalid_budget",
			fmt.Sprintf("health_timeout_s and swap_timeout_s must be between 0 and %d seconds (0 = use the project/node default)",
				int(maxBudget/time.Second)), nil)
		return
	}
	// An override image is written into the project's compose file or .env and then
	// pulled. Same reasoning as the budget check above — this endpoint takes a
	// replayed body with no bearer or mTLS of its own — but the consequence is
	// worse than a bad deadline: an unchecked value with a newline appends lines to
	// the stack's .env, and compose honours every one of them on the next `up`.
	if body.OverrideImage != "" && !validImageRef(body.OverrideImage) {
		refuse(c, http.StatusBadRequest, "invalid_override_image",
			"override_image must be a plain image reference (registry/name:tag[@sha256:…]); "+
				"whitespace, control characters and shell or interpolation metacharacters are refused", nil)
		return
	}
	// An op that builds from the compose files loads them now: a project whose files
	// do not load is refused here, coded, instead of starting a job that can only
	// fail. allowed_ops stays structural — loading every project on every fleet
	// snapshot would cost tens of milliseconds a project on each push.
	var project *types.Project
	if needsModel(body.Op) || body.Op == opComposeUpdate {
		loaded, err := a.compose.loadProject(c.Request.Context(), entry)
		if err != nil {
			refuse(c, http.StatusConflict, "project_load_failed",
				"the project's compose files do not load: "+err.Error(), gin.H{"working_dir": entry.WorkingDir})
			return
		}
		project = loaded
	}
	services, ok := a.checkOpServices(c, body, entry, summaries, project)
	if !ok {
		return
	}
	j := a.reg.start(context.Background(), JobRequest{
		Services: services,
		// A narrowed op's services ride in target — the column the controller's
		// docker_jobs history already has — so the history does not read whole-stack.
		Target:          strings.Join(services, ","),
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

// checkOpServices validates a service-narrowed op against the project as it loads
// now, and returns the list sorted and deduplicated. A false return has answered.
//
// down and restart act on containers, so their services are the project's
// containers' compose service labels (summaries, the fresh list the handler already
// took) — no files are read, and a project outside the compose root narrows them
// as it runs them whole. up, recreate and pull build from the model, so theirs are
// the services of the project as it loads; a project that does not load is a
// deterministic refusal, not a server error.
func (a *app) checkOpServices(c *gin.Context, body composeOpBody, entry ProjectEntry, summaries []container.Summary, project *types.Project) ([]string, bool) {
	if len(body.Services) == 0 {
		return nil, true
	}
	if body.Op == opComposeUpdate {
		refuse(c, http.StatusBadRequest, "invalid_body",
			"services does not apply to update, which targets one service with override_service", nil)
		return nil, false
	}
	services := slices.Compact(slices.Sorted(slices.Values(body.Services)))
	if len(services) > maxOpServices {
		refuse(c, http.StatusBadRequest, "invalid_body", fmt.Sprintf("at most %d services", maxOpServices), nil)
		return nil, false
	}
	for _, s := range services {
		if !composeServiceName.MatchString(s) {
			refuse(c, http.StatusBadRequest, "unknown_service",
				fmt.Sprintf("%s is not a valid compose service name", echo(s)), gin.H{"service": s})
			return nil, false
		}
	}
	known := map[string]bool{}
	if needsModel(body.Op) {
		for s := range project.Services { // loaded by the handler, which refused if it did not load
			known[s] = true
		}
	} else {
		for _, ctr := range summaries {
			if ctr.Labels[labelComposeProject] == entry.Name && ctr.Labels[labelComposeService] != "" {
				known[ctr.Labels[labelComposeService]] = true
			}
		}
	}
	for _, s := range services {
		if !known[s] {
			where := "a service of project " + echo(entry.Name)
			if !needsModel(body.Op) {
				where = "a service with containers in project " + echo(entry.Name)
			}
			refuse(c, http.StatusBadRequest, "unknown_service", fmt.Sprintf("%s is not %s", echo(s), where), gin.H{"service": s})
			return nil, false
		}
	}
	return services, true
}

// validBudgetSeconds reports whether a request-supplied budget is one the agent
// will honour. 0 stays legal — it is how a caller says "use the configured
// default" — so this rejects only negatives and anything past maxBudget.
func validBudgetSeconds(v int) bool {
	return v == 0 || (v > 0 && time.Duration(v)*time.Second > 0 && time.Duration(v)*time.Second <= maxBudget)
}

// capabilityOf is projectCapabilities for an entry the handlers resolved.
func (a *app) capabilityOf(e ProjectEntry, v *selfView) projectCapability {
	return projectCapabilities(e, a.cfg.ComposeRoot, v)
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
	// Owner rides the embedded ProjectEntry (`owner`), so it is already on this
	// row — it is the reason Managed is false while AllowedOps is full.
	// ServiceOps: every op in AllowedOps except update accepts "services" (see
	// projectCapability.serviceOps).
	ServiceOps bool `json:"service_ops"`
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
			Managed: capa.Editable, OpsBlocked: capa.Blocked, ServiceOps: capa.serviceOps(),
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
	// Replace must be true to register onto a project that already exists (see
	// refuseProjectExists). The Ansible module and the editor send it; a cross-host
	// copy onto an existing name does not.
	Replace bool `json:"replace,omitempty"`
	// Owner declares who renders this stack's files (ProjectEntry.Owner). A
	// POINTER because absent and empty mean different things on a replace: absent
	// KEEPS the existing owner — a re-register by anything that does not know
	// about ownership (a remediation, a copy) must not silently unclaim an
	// Ansible-rendered stack — and an explicit "" clears it, which is how a stack
	// stops being externally rendered.
	Owner *string `json:"owner,omitempty"`
}

// maxOwnerLen bounds ProjectEntry.Owner: it is a tool name that lands in refusal
// text and in projects.json, not free-form content.
const maxOwnerLen = 32

// validOwner reports whether owner is a name the agent will record. Same alphabet
// as a project name (lowercase, digits, '-', '_'), so it is readable in a message
// and safe in JSON; empty is valid and means "the agent owns it".
func validOwner(owner string) bool {
	if owner == "" {
		return true
	}
	if len(owner) > maxOwnerLen {
		return false
	}
	return validProjectName(owner)
}

func (a *app) handleRegisterProject(c *gin.Context) {
	var body registerProjectBody
	if err := c.ShouldBindJSON(&body); err != nil {
		refuse(c, http.StatusBadRequest, "invalid_body", "invalid request body: "+echo(err.Error()), nil)
		return
	}
	// Input first: a malformed request is a 400 whatever the capability would be.
	if body.Name == "" {
		refuse(c, http.StatusBadRequest, "invalid_project_name", "name is required", nil)
		return
	}
	if !validProjectName(body.Name) || len(body.Name) > maxProjectNameLen {
		refuseInvalidName(c, body.Name)
		return
	}
	if len(body.Files) == 0 && body.WorkingDir == "" {
		refuse(c, http.StatusBadRequest, "invalid_body", "working_dir or files is required", nil)
		return
	}
	if body.Owner != nil && !validOwner(*body.Owner) {
		refuse(c, http.StatusBadRequest, "invalid_owner",
			fmt.Sprintf("%q is not a valid owner: use lowercase letters, digits, '-' and '_', at most %d bytes "+
				"(or \"\" to clear it)", echo(*body.Owner), maxOwnerLen), nil)
		return
	}
	entry := ProjectEntry{
		Name:         body.Name,
		WorkingDir:   body.WorkingDir,
		ComposeFiles: body.ComposeFiles,
		Profiles:     body.Profiles,
		EnvFiles:     body.EnvFiles,
	}
	// An absent owner keeps what is recorded; the durable entry is the only
	// source (a live container label never carries one). prevOwner is who the
	// files belong to RIGHT NOW, which is what a write has to be checked against
	// — entry.Owner is already whoever this request says they will belong to.
	var prevOwner string
	if prev, ok := a.projects.get(body.Name); ok {
		prevOwner = prev.Owner
		entry.Owner = prev.Owner
	}
	if body.Owner != nil {
		entry.Owner = *body.Owner
	}
	if len(body.Files) > 0 {
		// Inline files always land under ComposeRoot; decide on that directory.
		entry.WorkingDir = filepath.Join(a.cfg.ComposeRoot, body.Name)
	}
	if !filepath.IsAbs(entry.WorkingDir) || len(entry.WorkingDir) > maxPathLen {
		refuse(c, http.StatusBadRequest, "invalid_working_dir",
			fmt.Sprintf("working_dir must be an absolute path of at most %d bytes", maxPathLen),
			gin.H{"working_dir": entry.WorkingDir})
		return
	}
	entry.WorkingDir = filepath.Clean(entry.WorkingDir)
	if err := validateDeclaredPaths(entry); err != nil {
		refuse(c, http.StatusBadRequest, "project_path_outside_working_dir", err.Error(), gin.H{"working_dir": entry.WorkingDir})
		return
	}
	if err := validateBundlePaths(body.Files); err != nil {
		refuse(c, http.StatusBadRequest, "invalid_file_path", err.Error(), nil)
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
	if existingDir, exists := a.existingProject(entry.Name, summaries); exists && !body.Replace {
		refuseProjectExists(c, entry.Name, existingDir)
		return
	}
	// Writing files into an owned stack requires SAYING who you are. The owner
	// pushing its own rendered files is the one legitimate case (Ansible sends
	// "owner": "ansible" with them); the editor, which sends no owner at all,
	// would otherwise overwrite files the next converge reverts. Taking the files
	// back is a path-only re-register with "owner": "" first, then the write.
	//
	// This is the cheap early answer, refusing before the lock wait. It is NOT the
	// decision — see the re-read under the lock below.
	if refuseOwnedWrite(c, body, entry, prevOwner, capa) {
		return
	}
	if len(body.Files) == 0 && capa.operable() {
		// A path-only register under ComposeRoot claims the file ops will run. Make
		// that claim true now, not an empty bundle discovered later. (Gated on
		// operable, not Editable: an externally-owned stack is not editable and its
		// files must still be there.)
		if err := composeFilesPresent(entry); err != nil {
			refuse(c, http.StatusBadRequest, "compose_files_missing", err.Error(), gin.H{"working_dir": entry.WorkingDir})
			return
		}
	}
	if body.Deploy && refuseProjectOp(c, entry, a.cfg.ComposeRoot, capa, opComposeUp) {
		return
	}
	release, locked := a.lockForRequest(c, projectLockKey(entry), "a register request for "+entry.Name)
	if !locked {
		return
	}
	defer release()
	// Under the directory's lock, so two registers naming one directory cannot
	// both pass this check.
	if owner, taken := a.projects.workingDirOwner(entry.WorkingDir, entry.Name); taken {
		refuseWorkingDirInUse(c, entry.WorkingDir, owner)
		return
	}
	// The owner is re-read HERE, under the lock, because the pre-lock snapshot is
	// stale by construction: the handler does a Docker list and can wait up to
	// requestLockWait for the lock, and a converge registering the stack in that
	// window would leave a concurrent editor save holding prevOwner == "" — writing
	// over the owner's files AND clearing the owner, the two things this rule
	// exists to stop, in one request.
	prevOwner = ""
	if prev, ok := a.projects.get(body.Name); ok {
		prevOwner = prev.Owner
		if body.Owner == nil {
			entry.Owner = prev.Owner // absent still means "keep what is recorded"
		}
	}
	if refuseOwnedWrite(c, body, entry, prevOwner, capa) {
		return
	}
	if len(body.Files) > 0 {
		dir, written, err := writeProjectFiles(a.cfg.ComposeRoot, body.Name, body.Files)
		if err != nil {
			// Paths were validated above, so what is left is I/O: transient.
			c.JSON(http.StatusInternalServerError, gin.H{"error": "write project files: " + echo(err.Error())})
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
	release() // the deploy job takes the lock itself, in turn
	resp := gin.H{"registered": entry.Name}
	if body.Deploy && a.compose != nil {
		j := a.reg.start(context.Background(), JobRequest{
			Operation: opComposeUp, Project: entry.Name, TriggerKey: "register",
		})
		resp["job_id"] = j.ID
	}
	c.JSON(http.StatusOK, resp)
}

// existingProject reports whether name already exists: registered, or running as
// containers labelled with that project. existingDir is its working dir.
func (a *app) existingProject(name string, summaries []container.Summary) (existingDir string, exists bool) {
	if e, ok := a.projects.get(name); ok {
		return e.WorkingDir, true
	}
	for _, p := range groupComposeProjects(summaries) {
		if p.Name == name {
			return p.WorkingDir, true
		}
	}
	return "", false
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
		refuse(c, http.StatusNotFound, "unknown_project", "unknown project: "+echo(name), nil)
		return
	}
	view, ok := a.viewForRead(c)
	if !ok {
		return
	}
	if !a.capabilityOf(entry, view).Readable {
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
		refuse(c, http.StatusBadRequest, "invalid_body", "invalid request body: "+echo(err.Error()), nil)
		return
	}
	if body.NewName == "" {
		refuse(c, http.StatusBadRequest, "invalid_project_name", "new_name is required", nil)
		return
	}
	if !validProjectName(body.NewName) || len(body.NewName) > maxProjectNameLen {
		refuseInvalidName(c, body.NewName)
		return
	}
	if existing, exists := a.projects.get(body.NewName); exists {
		refuseProjectExists(c, body.NewName, existing.WorkingDir)
		return
	}
	src, ok := a.projects.get(name)
	if !ok {
		refuse(c, http.StatusNotFound, "unknown_project", "unknown project: "+echo(name), nil)
		return
	}
	_, view, proceed := a.selfForMutation(c)
	if !proceed {
		return
	}
	// A copy READS the source's files, so the source must be readable — an
	// externally-owned source is fine, the copy lands as a new project this agent
	// owns. The copy must not take one of the agent's own project names, whose `up`
	// would recreate — or remove as an orphan — the agent itself. Both refusals
	// precede any write.
	if refuseProjectRead(c, src, a.cfg.ComposeRoot, a.capabilityOf(src, view)) {
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
	// The source is read under its lock, so the copy never takes a compose file
	// from before a running update's write and an env file from after it.
	releaseSrc, locked := a.lockForRequest(c, projectLockKey(src), "a copy request from "+src.Name)
	if !locked {
		return
	}
	bundle, err := a.readBundle(src)
	releaseSrc()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	release, locked := a.lockForRequest(c, projectLockKey(dst), "a copy request to "+dst.Name)
	if !locked {
		return
	}
	defer release()
	if owner, taken := a.projects.workingDirOwner(dst.WorkingDir, dst.Name); taken {
		refuseWorkingDirInUse(c, dst.WorkingDir, owner)
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write project files: " + echo(err.Error())})
		return
	}
	entry := ProjectEntry{Name: body.NewName, WorkingDir: dir, ComposeFiles: composeFileNames(written)}
	if err := a.projects.register(entry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	release()
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
	// The same files a compose load reads (resolveLoadPaths), so the editor shows
	// what the stack actually runs from.
	paths, err := resolveLoadPaths(e)
	if err != nil {
		return b, nil // no compose file: an empty bundle, as before
	}
	for _, p := range paths.config {
		if content, ok := read(p); ok {
			b.ComposeFiles = append(b.ComposeFiles, bundleFile{Name: filepath.Base(p), Content: content})
		}
	}
	for _, p := range paths.env {
		if content, ok := read(p); ok {
			b.EnvFiles = append(b.EnvFiles, bundleFile{Name: filepath.Base(p), Content: content})
		}
	}
	return b, nil
}

// writeProjectFiles writes a bundle's files under <root>/<name>/, rejecting any
// path that escapes the project dir. Returns the created dir + the relative
// names written.
// requestLockWait bounds how long a register or copy request waits for its
// project's lock before answering 409 project_busy: long enough for a restart or
// a quick up, far shorter than an update, which can hold it for many minutes.
var requestLockWait = 10 * time.Second

// lockForRequest takes project's lock for a synchronous request. When it cannot
// within requestLockWait (or the caller goes away), the request has been answered
// and locked is false.
func (a *app) lockForRequest(c *gin.Context, key, who string) (release func(), locked bool) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestLockWait)
	defer cancel()
	holder := "another change"
	release, err := a.reg.eng.locks.acquire(ctx, key, who, func(h string) { holder = h })
	if err != nil {
		refuseProjectBusy(c, strings.TrimPrefix(strings.TrimPrefix(key, "dir:"), "name:"), holder)
		return nil, false
	}
	return release, true
}

// writeProjectFiles writes inline files under root/name. Each file is written
// atomically, and only where it resolves inside the compose root: a symlink left
// in the project directory is not a way to write outside it.
func writeProjectFiles(root, name string, files map[string]string) (string, []string, error) {
	if root == "" {
		return "", nil, fmt.Errorf("compose root not configured")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", nil, fmt.Errorf("invalid project name %q", echo(name))
	}
	if err := validateBundlePaths(files); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	var written []string
	for rel, content := range files {
		clean := cleanBundlePath(rel)
		full := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", nil, err
		}
		if err := confinePath(full, root); err != nil {
			return "", nil, err
		}
		if err := writeFileAtomic(full, []byte(content), 0o644); err != nil {
			return "", nil, err
		}
		written = append(written, clean)
	}
	return dir, written, nil
}

func cleanBundlePath(rel string) string {
	return strings.TrimPrefix(filepath.Clean("/"+rel), "/")
}

// validateBundlePaths is the deterministic half of writing a bundle: every file
// path stays inside the project dir and fits the filesystem. What fails after it
// is I/O.
func validateBundlePaths(files map[string]string) error {
	for rel := range files {
		clean := cleanBundlePath(rel)
		if clean == "" || clean == "." || strings.HasPrefix(clean, "..") || len(clean) > maxPathLen {
			return fmt.Errorf("invalid file path %q", echo(rel))
		}
		for _, part := range strings.Split(clean, "/") {
			if len(part) > maxProjectNameLen {
				return fmt.Errorf("invalid file path %q: a component exceeds %d bytes", echo(rel), maxProjectNameLen)
			}
		}
	}
	return nil
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
