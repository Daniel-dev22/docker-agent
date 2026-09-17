# Agent stack ops — plan

**Goal:** Docker Jobs only ever offers an operation the host's docker-agent can actually run, and
the Ansible-owned agent stacks (duplicacy-agent-api, gdrive-agent, build-agent, filemesh-agent) can
be updated from Docker Jobs. docker-agent itself stays Ansible-only (user decision, 2026-09-16).

Spans three repos: docker-agent (this one), control-center, ansible.

## Phases

| Phase | Ships | Status |
|---|---|---|
| 1 | Truthful per-op capability in the agent, advertised on the wire and enforced; consumers (UI, Update Stack, control-api, sync-worker, system_monitor, Ansible modules) act on it; deploy-scope fix so the VPS agent stops falling behind | ✅ done — `docs/AGENT_STACK_OPS_PHASE1_HANDOFF.md` |
| 2 | The four Ansible-owned agent stacks relocated under COMPOSE_ROOT, registered `owner: ansible` (operable, not editable), image pin in their `.env` | next |

## Phase 2 design (updated after Phase 1)

- **Agent:** `ProjectEntry.Owner` persisted; `POST /v1/projects` accepts `"owner"`; `managed`
  (editable) = under root ∧ not self ∧ valid name ∧ `Owner == ""`. Operability unchanged — an
  Ansible-owned stack under the root is operable. Contract: update `docker_agent_client.py`
  (`register_project(..., replace, owner)`) and the `docker_agent_stack` module first; verify both
  consumers (module + system_monitor).
- **Each role** (duplicacy-agent-api, gdrive-agent, build-agent, filemesh-agent): template the compose
  to `{{ docker_agent_data_dir }}/<agent>/docker-compose.yml` with top-level `name: <agent>`; write
  `CURRENT_<AGENT>_IMAGE` into that directory's `.env` (not the task's process env — a pin in a
  deploy tool's process env is invisible to every other runner); `compose up` from the new path
  (relabels the containers); register path-only with `replace: true, owner: ansible`, failing the
  role if registration fails; remove the old compose file.
- **Shared-object audit:** every reference to `{{vol}}/<agent>/docker-compose.yml`, starting with
  `server_setup/general/include_tasks/duplicacy_restore.yaml` (DR start path).
- **Already true after Phase 1** (do not rebuild): `replace` exists; relative `.env`/env_files resolve
  against the working dir; tag writes are in place + reload-verified + byte-exact revert; include/extends
  confinement; per-project lock keyed by directory; `working_dir_in_use`; service-targeted ops.
- **Registry junk to clear first (user OK):** `homeassistant` entries on kd-nuc02/kd-pi01/kd-vm01
  (leftover traefik compose), kd-nuc01 `myapp`, dead `duplicacy` on 6 hosts.
- **Verify:** Docker Jobs `update` of gdrive-agent on one host completes with the health check and
  the `.env` pin moves; a bad `override_image` rolls back; the role re-run is idempotent; the stack
  shows `allowed_ops ⊇ {update}`, `managed:false`.

## Deferred register

| Item | Reason | Measurement / trigger |
|---|---|---|
| Job-history retention (agents' job tables; controller `docker_jobs`, `duplicacy_jobs`) | pre-existing, fleet-wide; raised with user as its own phase | no DELETE path in any; only `build_jobs` bounded |
| Idempotency middleware → agent-kit-go | only docker-agent has a POST-resending consumer | promote the day a second agent gets one |
| system-monitor CI job failures (missing cryptography/discord/systemd in requirements, gitignored pysyncobj, stuck_intent_reconciler) | pre-existing, unrelated | 95 failed / 51 errors on the reproduced job |
| Router tests failing on main (providerset DefaultClient; push-api roots/establish route) | not this work | fail identically on origin/main |
| Dead `duplicacy` project registered on 6 hosts | cleanup needs user OK | 0 containers |
| GitHub Actions billing-blocked | user action | runs don't start |
