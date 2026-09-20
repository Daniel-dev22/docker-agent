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
| 3 | **Ownership survives losing the registry** — `Owner` stops being a fact that exists only in `projects.json`, so an agent that loses it cannot silently reopen the editor on Ansible-rendered files | next |
| 4 | **traefik joins the owned set** — the fifth Ansible-rendered stack under the root declares `owner: ansible`, and the read-modify-write callers that would break are fixed first | 🔒 GATED, see below |

### Phase 3 — what it ships, and the one thing to do first

One sentence: *after Phase 3, deleting `projects.json` on a live host cannot make an
Ansible-rendered stack editable.*

Do the reproduction BEFORE choosing a design — the fix depends on what the agent actually does,
and this has not been observed, only reasoned about. On a throwaway host with the stacks running:
`rm` the agent's `projects.json`, restart it, `GET /v1/projects`. The expectation is `managed:true`
and no `owner` on all four.

Then choose, and write down why the other was rejected:

- **(a) a compose label `projectEntryFromLive` reads back.** Structural and self-correcting, the
  same shape as `underComposeRoot`. Cost: a SECOND source of truth for one fact — the template and
  the register both declare the owner and can disagree. That is the reason it was not simply done
  in Phase 2.
- **(b) make the loss loud rather than silent.** Readiness reports "adopted entries under the root
  with no recorded owner". Cheaper, no second source, but it leaves the window open and relies on
  someone reading readiness.

Also in scope, because it is the same fact: `deregister` takes no lock and no owner check. With
structural recovery it cannot launder ownership; without it, it can.

Verify while there: `nut-ups` should by then have taken its owner at its next template change —
confirm, because the nut path is the one Phase 2 broke and fixed, and it is still unexercised in
production.

### Phase 4 — gated, and the gate is not a preference

traefik is the fifth Ansible-rendered stack and already sits under the ComposeRoot, so declaring
it owned is a two-line change. **Do not do it until the three literal secrets in its compose are
moved to env from Bitwarden and rotated** (see the register). Ownership would not close that
exposure — reads are governed by containment — so declaring it owned would look like a fix while
changing nothing about the secrets.

The other reason to take them in this order: `server_setup/general/include_tasks/rewrite_stack_container_dns.yaml`
reads a stack's bundle and pushes it back, and its `dns_pinning_stacks` is `[traefik,
homeassistant]`. It is inert today only because neither is owned. It is the same read-modify-write
shape that broke `nut-ups`, and it fails CLOSED (a silent no-op) rather than loudly. Fix it in the
same change that declares traefik owned.

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
  - ⚠ **CORRECTED in review.** `managed` = editable was only half the model. Ownership governs
    WRITES; the COMPOSE ROOT governs reads. Gating `/bundle` on editability broke the nut stack's
    own converge, which reads its stack back to preserve the `.env` — see the Phase 2 handoff's
    Surprises. `Readable` and `Editable` are separate.
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

**The one register.** Phases 1 and 2 each kept their own and they drifted; these are merged, and
a row's reason is re-measured when it is picked up, not trusted.

### Needs a decision that is the user's

| Item | Reason / measurement | State |
|---|---|---|
| 🔴 **Literal secrets in the traefik compose, served by `/bundle` on every host.** A Cloudflare DNS API token, an AWS access key id and an AWS secret, as literal values — not `${VAR}` references. | Raised in Phase 1 as "belongs to the traefik role". **Phase 2 made it concrete and it was then MEASURED: readable on 10/10 hosts (kd-vps01 carries 2 of the 3).** The agent API is unauthenticated on the host's docker network, and Phase 2 established that bundle readability is governed by CONTAINMENT — traefik's compose sits under the ComposeRoot, so it is served. Declaring traefik `owner: ansible` would NOT close this: ownership governs writes. | **OPEN — the sharpest item here.** Fix is to move the three values to env from Bitwarden and rotate them, in the traefik role. |
| `filemesh-agent` is not built for nghome, yet its role targets ng-nas01 | pre-existing; no `filemesh-agent` repo in the ng registry, no container has ever run there. The role has always failed at the image pull. Phase 2 changed only the footprint of that failure (it now registers before the pull, so a phantom entry is created — cleaned up 2026-09-20). | OPEN — build it for ng, or exclude ng from the role's targeting |
| Job-history retention (agents' job tables; controller `docker_jobs`, `duplicacy_jobs`) | pre-existing, fleet-wide across 5 repos; no DELETE path in any of them, only `build_jobs` is bounded | OPEN — raised as its own phase |
| GitHub Actions billing-blocked | runs do not start, so every CI job is reproduced locally | OPEN — user action |

### Queued engineering

| Item | Reason / measurement | State |
|---|---|---|
| **Ownership has no structural recovery.** `Owner` lives only in `projects.json`; lose it while containers run and `enrichFromLive` re-adopts them unowned → `managed:true` → the editor reopens on Ansible's files until the next converge. | The obvious fix (a compose label read back) is a SECOND source of truth for one fact. Trigger is narrow: the ComposeRoot must be wiped *while the containers keep running*. Reproduce by deleting `projects.json` and restarting the agent with the stacks up. | OPEN — **Phase 3's first step** |
| `deregister` takes no lock and no owner check | subsumed by the row above: with structural recovery a deregister cannot launder ownership. API-only; nothing in the UI calls it. | OPEN, blocked on the above |
| `nut-ups` has not taken its owner yet | its register is gated `when: _nut_compose_changed`, so the owner is first set at its next genuine template change. Correct and safe now reads are open — but the nut path is not yet exercised in production. | OPEN — check `owner` on a nut host after its next template change |
| `rewrite_stack_container_dns.yaml` silently no-ops on an owned stack | inert today: `dns_pinning_stacks` is `[traefik, homeassistant]`, neither owned. Becomes real the day traefik is declared owned. | OPEN, inert |
| Idempotency middleware → `agent-kit-go` | only docker-agent has a POST-resending consumer | OPEN — promote the day a second agent gets one |

### Pre-existing test/CI noise — each verified against a baseline, none caused by this work

| Item | Baseline that proves it is pre-existing |
|---|---|
| system-monitor CI job failures (cryptography/discord/systemd missing from requirements; gitignored pysyncobj; stuck_intent_reconciler) | 95 failed / 51 errors on the reproduced job |
| Two router tests red on main (price-monitor providerset DefaultClient; push-api roots/establish route) | fail identically on `origin/main` |
| `test_docker_redeploy_network_stacks.py::test_read_engine…` | fails only in a full-suite run; passes alone; **fails identically on `main`** |
| `test_bond_migration_playbook.py::test_every_jinja_filter_…` | fails only in the full CI invocation; **fails identically at the previous release tag `51.107.7`**; passes alone (114/114) and with the filter tests (491/491) |
| `plugins/modules/tests` + `plugins/module_utils/tests` `conftest.py` collision → 11 collection errors | CI passes an explicit file list, which is why CI never sees it |

### Closed

| Item | How |
|---|---|
| ~~esphome image-ID pin on kd-nuc01 / ng-nuc01~~ | the user repaired both hosts themselves, 2026-09-18 |
| ~~Dead `duplicacy` project registered on 6 hosts~~ | cleared 2026-09-20 (user approved) |
| ~~`homeassistant` junk on kd-nuc02/kd-pi01/kd-vm01 + kd-nuc01 `myapp`~~ | cleared 2026-09-20. ⚠ **The Phase 1 row was wrong by omission**: `homeassistant` is ALSO registered on kd-nuc01 and ng-nuc01, where it is RUNNING with a live container. Those two were left alone. Container counts were checked per entry before deleting. |
