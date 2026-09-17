package main

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/gin-gonic/gin"
)

// Why a project's allowed ops are not the full set. Also the wire value of
// ComposeProject.OpsBlocked, reported in this priority order.
const (
	blockedSelf               = "self"
	blockedInvalidName        = "invalid_name"
	blockedOutsideComposeRoot = "outside_compose_root"
	blockedControlPath        = "control_path"
)

// labelOps are rebuilt by compose from the live containers' labels — they never
// read the project's files. fileOps load the compose model, so the files must be
// visible inside the agent's mount.
var (
	labelOps = []string{opComposeDown, opComposeRestart}
	fileOps  = []string{opComposePull, opComposeRecreate, opComposeUp, opComposeUpdate}
)

// projectCapability is what the agent can honestly do with a compose project.
type projectCapability struct {
	Allowed  []string // sorted subset of projectOps; never nil
	Editable bool     // read/rewrite its files (edit, copy source)
	Blocked  string   // the highest-priority reason Allowed is not every op; "" when it is
	// controlPath: this project runs the agent's control-path container, which is
	// why `down` is missing even when Blocked names a higher-priority reason.
	controlPath bool
}

func (c projectCapability) allows(op string) bool {
	for _, a := range c.Allowed {
		if a == op {
			return true
		}
	}
	return false
}

// operable is the wire `operable`: the file-loading ops (up/pull/recreate/update)
// are allowed.
func (c projectCapability) operable() bool { return c.allows(opComposeUpdate) }

// validProjectName is compose's own rule. compose-go refuses any other name when a
// project is loaded, and Down/Restart LOWERCASE it before acting — so "Docker-Agent"
// would act on the project "docker-agent". A name only compose can reinterpret is a
// name no op may run under.
func validProjectName(name string) bool {
	return name != "" && loader.NormalizeProjectName(name) == name
}

// projectCapabilities is the ONE decision about a project. The fleet snapshot, GET
// /v1/projects and every mutating endpoint read it, so a consumer keyed on what the
// agent advertises can never offer an op the endpoint refuses.
//
//   - The agent's own stack allows nothing, wherever its files live: compose would
//     stop or orphan-remove the container running the job. Self is keyed on the
//     live containers' project labels (selfView), so re-registering a working dir
//     cannot launder it.
//   - A name compose would normalise allows nothing (validProjectName).
//   - restart and down always work otherwise: compose rebuilds them from labels.
//   - up/pull/recreate/update need the compose files, which the agent can see only
//     under ComposeRoot. The registry entry's working dir decides — it is what an op
//     loads.
//   - The project running the agent's control-path container loses `down`: nothing
//     could bring it back, and the agent would be unreachable until it was.
func projectCapabilities(name, workingDir, composeRoot string, v *selfView) projectCapability {
	if v.isSelfProject(name) {
		return projectCapability{Allowed: []string{}, Blocked: blockedSelf}
	}
	if !validProjectName(name) {
		return projectCapability{Allowed: []string{}, Blocked: blockedInvalidName}
	}
	c := projectCapability{}
	allowed := append([]string{}, labelOps...)
	if underComposeRoot(workingDir, composeRoot) {
		allowed = append(allowed, fileOps...)
		c.Editable = true
	} else {
		c.Blocked = blockedOutsideComposeRoot
	}
	if v.isControlPathProject(name) {
		c.controlPath = true
		kept := allowed[:0]
		for _, op := range allowed {
			if op != opComposeDown {
				kept = append(kept, op)
			}
		}
		allowed = kept
		if c.Blocked == "" {
			c.Blocked = blockedControlPath
		}
	}
	sort.Strings(allowed)
	c.Allowed = allowed
	return c
}

// refuseProjectOp writes the 409 when op is not allowed on the project. Returns
// true when it refused (the caller must return).
func refuseProjectOp(c *gin.Context, e ProjectEntry, composeRoot string, capa projectCapability, op string) bool {
	if capa.allows(op) {
		return false
	}
	if capa.Blocked == blockedSelf {
		refuseSelfProject(c, e)
		return true
	}
	var msg string
	switch {
	case capa.Blocked == blockedInvalidName:
		msg = fmt.Sprintf("%q is not a valid compose project name: compose would act on %q instead. "+
			"Deregister it and register the stack under a lowercase name.", e.Name, loader.NormalizeProjectName(e.Name))
	case op == opComposeDown && capa.controlPath:
		msg = fmt.Sprintf("%s runs this agent's control-path proxy: `down` would leave the agent unreachable "+
			"with nothing able to start it again. restart, recreate and update are allowed.", e.Name)
	default:
		msg = fmt.Sprintf("compose files for %s are at %s, outside docker-agent's compose root (%s): %s loads "+
			"the compose files, which the agent cannot see. restart and down do not read files and are allowed.",
			e.Name, e.WorkingDir, composeRoot, op)
	}
	c.JSON(http.StatusConflict, gin.H{"error": msg, "code": "project_not_operable", "working_dir": e.WorkingDir})
	return true
}

// refuseProjectEdit writes the 409 when the project's files cannot be read or
// rewritten here (edit, copy source).
func refuseProjectEdit(c *gin.Context, e ProjectEntry, composeRoot string, capa projectCapability) bool {
	if capa.Editable {
		return false
	}
	if capa.Blocked == blockedSelf {
		refuseSelfProject(c, e)
		return true
	}
	msg := fmt.Sprintf("compose files for %s are at %s, outside docker-agent's compose root (%s), "+
		"so they cannot be read or rewritten here", e.Name, e.WorkingDir, composeRoot)
	if capa.Blocked == blockedInvalidName {
		msg = fmt.Sprintf("%q is not a valid compose project name", e.Name)
	}
	c.JSON(http.StatusConflict, gin.H{"error": msg, "code": "project_not_editable", "working_dir": e.WorkingDir})
	return true
}

func refuseSelfProject(c *gin.Context, e ProjectEntry) {
	c.JSON(http.StatusConflict, gin.H{
		"error": fmt.Sprintf("%s is docker-agent's own stack: acting on it would stop or remove the agent mid-job, "+
			"leaving the host with no agent. The agent is updated by Ansible (docker-agent/deploy.yml).", e.Name),
		"code":        "self_project",
		"working_dir": e.WorkingDir,
	})
}

func refuseInvalidName(c *gin.Context, name string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error": fmt.Sprintf("%q is not a valid compose project name: use lowercase letters, digits, '-' and '_', "+
			"starting with a letter or digit (compose would act on %q)", name, loader.NormalizeProjectName(name)),
		"code": "invalid_project_name",
	})
}

func refuseSelfUnavailable(c *gin.Context, err error, targets []string) {
	body := gin.H{
		"error": "cannot confirm this request does not target docker-agent itself: " + err.Error() +
			". Retry once the Docker daemon answers.",
		"code": "self_identity_unavailable",
	}
	if targets != nil {
		body["targets"] = targets
	}
	c.JSON(http.StatusServiceUnavailable, body)
}
