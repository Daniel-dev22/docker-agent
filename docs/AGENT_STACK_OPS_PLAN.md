# Agent stack ops — plan

**Goal:** Docker Jobs only ever offers an operation the host's docker-agent can actually run, and
the Ansible-owned agent stacks (duplicacy-agent-api, gdrive-agent, build-agent, filemesh-agent) can
be updated from Docker Jobs. docker-agent itself stays Ansible-only (user decision, 2026-09-16).

Spans three repos: docker-agent (this one), control-center, ansible.

## Phases

| Phase | Ships | Status |
|---|---|---|
| 1 | Truthful per-op capability in the agent, advertised on the wire and enforced; consumers (UI, Update Stack, control-api, sync-worker, system_monitor, Ansible modules) act on it; deploy-scope fix so the VPS agent stops falling behind | ✅ done — `docs/AGENT_STACK_OPS_PHASE1_HANDOFF.md` |
| 2 | The four Ansible-owned agent stacks relocated under COMPOSE_ROOT, registered `owner: ansible` (operable, not editable), image pin in their `.env` | ✅ done — `docs/AGENT_STACK_OPS_PHASE2_HANDOFF.md` |

## Phase 2 design (as BUILT — corrections to the pre-build design are marked)

- **Agent:** `ProjectEntry.Owner` persisted; `POST /v1/projects` accepts `"owner"`; `managed`
  (editable) = under root ∧ not self ∧ valid name ∧ `Owner == ""`. Operability unchanged — an
  Ansible-owned stack under the root is operable. Contract: update `docker_agent_client.py`
  (`register_project(..., replace, owner)`) and the `docker_agent_stack` module first; verify both
  consumers (module + system_monitor).
  - ⚠ **CORRECTED while building.** `owner` is a POINTER (`*string`) on the wire: absent PRESERVES
    the recorded owner, `""` clears it. A flat string would have let system_monitor's remediation
    and a cross-host copy — neither of which knows about ownership — unclaim an Ansible stack on
    any re-register.
  - ⚠ **CORRECTED while building.** The rule is not "an owned stack refuses inline `files`" but
    "an inline-`files` register onto an owned stack must DECLARE that same owner". The first
    version would have locked out the one caller that legitimately writes those files — the owner.
    It also let a DIFFERENT owner take over, because the comparison was against the resulting
    owner rather than the previous one; the test written for the rule caught its own fix.
  - ⚠ **CORRECTED while building.** The compose-files-present check on a path-only register moves
    from `capa.Editable` to `capa.operable()`, which is the set `Editable` used to mean. Left on
    `Editable`, an owned register would have been the one path that can claim operable with
    nothing to load.
  - The client also gained `working_dir`/`compose_files`/`env_files`: the module only had the
    INLINE register mode, so path-only registration did not exist for Ansible at all.
- **Each role** (duplicacy-agent-api, gdrive-agent, build-agent, filemesh-agent): template the compose
  to `{{ docker_agent_data_dir }}/<agent>/docker-compose.yml` with top-level `name: <agent>`; write
  `CURRENT_<AGENT>_IMAGE` into that directory's `.env` (not the task's process env — a pin in a
  deploy tool's process env is invisible to every other runner); `compose up` from the new path
  (relabels the containers); register path-only with `replace: true, owner: ansible`, failing the
  role if registration fails; remove the old compose file.
  - ⚠ **CORRECTED while building.** Not four copies: ONE shared
    `docker-agent/include_tasks/converge_owned_stack.yaml` that each role calls with a `vars:`
    block. Six drifting copies of `upsert-agent-token.yml` is why that is a rule here.
  - ⚠ **The `.env` is SEEDED, not rendered.** `.env` is the file the agent's `update` writes into,
    so re-rendering it every converge would roll a Docker Jobs update back to whatever tag the
    playbook resolved — which, with no `-e`, is `latest`. Ansible writes the pin when the key is
    absent, and rewrites it only for an explicit `-e <agent>_image_tag`. That rule, and its
    reason, already existed in `ups/include_tasks/deploy_nut_stack.yaml`.
  - **Only the compose FILE moves.** The legacy directory keeps `state/`, `logs/`, `restores/` and
    the stack's `bearer-token`, all bind-mounted by absolute host path.
- **Shared-object audit:** every reference to `{{vol}}/<agent>/docker-compose.yml`, starting with
  `server_setup/general/include_tasks/duplicacy_restore.yaml` (DR start path).
  - ⚠ **The plan's premise here was wrong, and something worse was true.** `duplicacy_restore.yaml`
    does NOT reference any of the four stacks' compose paths — it reads the agent's *state* dir.
    But its traefik start pointed at `{{vol}}/traefik`, whose `docker-compose.yaml` the EARLIER
    traefik relocation deletes. The same class of miss, already realised, on a path that only runs
    when a host is broken. Fixed in this phase.
  - A fifth Ansible-rendered stack exists — `nut-ups` — and had the same latent bug (the UI offers
    Edit on files the role reverts). It declares `owner: 'ansible'` now.
- **Already true after Phase 1** (do not rebuild): `replace` exists; relative `.env`/env_files resolve
  against the working dir; tag writes are in place + reload-verified + byte-exact revert; include/extends
  confinement; per-project lock keyed by directory; `working_dir_in_use`; service-targeted ops.
- **Registry junk to clear first:** `homeassistant` entries on kd-nuc02/kd-pi01/kd-vm01
  (leftover traefik compose), kd-nuc01 `myapp`, dead `duplicacy` on 6 hosts. **User approved
  clearing ALL of it, 2026-09-18.**
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
| ~~Dead `duplicacy` project registered on 6 hosts~~ | CLOSED — user approved clearing 2026-09-18, done in Phase 2 | 0 containers |
| GitHub Actions billing-blocked | user action | runs don't start |
