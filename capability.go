package main

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Why a project is not operable. Also the wire value of ComposeProject.OpsBlocked.
const (
	blockedSelf               = "self"
	blockedOutsideComposeRoot = "outside_compose_root"
)

// projectCapability is what the agent can honestly do with a compose project.
type projectCapability struct {
	Operable bool   // compose ops (up/down/pull/restart/recreate/update)
	Editable bool   // read/rewrite its files (edit, copy)
	Blocked  string // blockedSelf | blockedOutsideComposeRoot | ""
}

// projectCapabilities is the ONE decision about a project — the fleet snapshot
// advertises it and every mutating endpoint enforces it, so a UI keyed on the
// snapshot can never offer an op the endpoint refuses.
//
//   - The agent's own stack is never operable, wherever its files live: compose
//     would stop the container running the job. This is checked FIRST so the rule
//     does not depend on the agent's compose file happening to sit outside
//     ComposeRoot.
//   - Otherwise a stack is operable only when its working dir is under ComposeRoot:
//     that is the only compose content the container can see, and compose must read
//     the files to load the project.
//
// Editable currently equals Operable — both need the files to be visible.
func projectCapabilities(name, workingDir, composeRoot, selfProject string) projectCapability {
	if selfProject != "" && name == selfProject {
		return projectCapability{Blocked: blockedSelf}
	}
	if !underComposeRoot(workingDir, composeRoot) {
		return projectCapability{Blocked: blockedOutsideComposeRoot}
	}
	return projectCapability{Operable: true, Editable: true}
}

// refuseProject writes the 409 for a project the requested action cannot run on.
// needEdit selects the edit/copy wording; otherwise it is a compose-op refusal.
// Returns true when it refused (the caller must return).
func refuseProject(c *gin.Context, name, workingDir, composeRoot string, capa projectCapability, needEdit bool) bool {
	switch {
	case capa.Blocked == blockedSelf:
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("%s is docker-agent's own stack: stopping or recreating it would kill the agent mid-job, "+
				"leaving the host with no agent. The agent is updated by Ansible (docker-agent/deploy.yml).", name),
			"code":        "self_project",
			"working_dir": workingDir,
		})
		return true
	case needEdit && !capa.Editable:
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("compose files for %s are at %s, outside docker-agent's compose root (%s), "+
				"so they cannot be read or rewritten here", name, workingDir, composeRoot),
			"code":        "project_not_editable",
			"working_dir": workingDir,
		})
		return true
	case !needEdit && !capa.Operable:
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("compose files for %s are at %s, outside docker-agent's compose root (%s), "+
				"so it cannot run compose ops on this stack", name, workingDir, composeRoot),
			"code":        "project_not_operable",
			"working_dir": workingDir,
		})
		return true
	}
	return false
}
