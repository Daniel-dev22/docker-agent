# Agent stack ops — Phase 2 handoff: the agent stacks move under the root, and Ansible says it renders them

Plan: `docs/AGENT_STACK_OPS_PLAN.md` (this repo), updated to match what was actually built —
six of its design bullets were wrong or incomplete and are marked ⚠ there.
Phase 1: `docs/AGENT_STACK_OPS_PHASE1_HANDOFF.md`.

## Status

| | docker-agent | ansible | control-center |
|---|---|---|---|
| Branch | `feat/project-owner` | `feat/agent-stacks-under-compose-root` | `feat/docker-stack-owner` |
| Commits | 5 | 3 | 3 |
| Released / deployed | see §Rollout record | see §Rollout record | see §Rollout record |
| Review | 4 independent lenses + a 33-mutation sweep; every finding fixed, every survivor closed and re-killed |

**If §Rollout record is missing below, this was merged but NOT deployed.**

## Why this phase exists

Docker Jobs `update` was refused on all four Ansible-rendered agent stacks
(`duplicacy-agent-api`, `gdrive-agent`, `build-agent`, `filemesh-agent`) with
`409 project_not_operable`: the agent loads compose files only inside its own bind mount, and
these were rendered outside it. Moving them in is the whole fix — but being under the root was
*also* what made a stack editable, so the move alone would have offered Edit and Duplicate on
files Ansible overwrites on its next converge.

## What shipped

### docker-agent — `ProjectEntry.Owner`, and the read/write split

- **`Owner`** names the tool that RENDERS a stack's files (`"ansible"`). Persisted in
  `projects.json`, on the wire as `owner` (omitempty), bounded to 32 bytes of `[a-z0-9_-]`.
- **`projectCapabilities` now answers two questions**, and conflating them was a real bug (below):
  - `Readable` = under the compose root — the agent can SEE the files. `/bundle` and the copy
    source gate on this. An owned stack IS readable.
  - `Editable` = `Readable ∧ Owner == ""` — the agent may REWRITE them. The editor gates on this;
    it is what `managed` carries on the wire.
- It takes a `ProjectEntry` rather than four positional strings, so the owner cannot be dropped at
  one of its three call sites.
- **`owner` is reported whatever the verdict** (self, invalid name, control path): it is a fact
  about the entry, not a reason an op is blocked. `ops_blocked` stays EMPTY for an owned stack —
  nothing about its ops is blocked — and `owner` is the field that explains the lock.
- **Register**: `owner` is a `*string`. Absent PRESERVES the recorded owner; `""` clears it.
  A register carrying inline `files` onto an owned stack must DECLARE that same owner. The owner
  is re-read **under the project's lock**, because the pre-lock snapshot is stale by construction.
- New codes: `project_owned`, `invalid_owner`. `project_not_editable` → **`project_not_readable`**
  (it refuses a read; nothing branched on the old value — `shared/refusal` keys on *having* a code).

### ansible

- **One shared include**, `docker-agent/include_tasks/converge_owned_stack.yaml`; each of the four
  roles calls it with a `vars:` block. It renders the compose to
  `{{ docker_agent_data_dir }}/<agent>/docker-compose.yml`, seeds the image pin in that directory's
  `.env`, registers path-only with `owner: 'ansible'`, and removes the legacy compose **file**
  (never the directory — it still holds `state/`, `logs/`, `restores/` and the bearer token).
- It **loads the ComposeRoot definition itself**, namespaced, rather than making four callers
  remember a `vars_files` line.
- **The `.env` is seeded, not rendered.** `.env` is where the agent's `update` writes the new
  image, so re-rendering it every converge would roll a Docker Jobs update back to whatever the
  playbook resolves — with no `-e`, `latest`. Ansible writes the pin only when no non-empty value
  is there, or when an operator passed `-e <agent>_image_tag`.
- The compose templates pin `name: <stack>` so identity does not depend on the directory.
- `docker_agent_client.register_project` gained `working_dir`/`compose_files`/`env_files`/`owner`
  (the module had only the INLINE mode — path-only registration did not exist for Ansible at all);
  `docker_agent_stack` gained the matching params, `required_if` any-of and `mutually_exclusive`.
- `ups/include_tasks/deploy_nut_stack.yaml` declares `owner: 'ansible'` — the fifth
  Ansible-rendered stack, which has had the "UI offers Edit on files the role reverts" bug all along.
- **Pre-existing bug fixed:** `traefik_compose_directory` pointed at
  `{{vol}}/traefik`, whose `docker-compose.yaml` the traefik relocation DELETES. Its only consumer
  is the DR start path in `duplicacy_restore.yaml` — a path that runs when a host is already broken,
  with nothing to bring up.

### control-center

`ComposeProject.owner`; the locked-Edit tooltip and the heading pill name the owner and say the
compose ops still work. No router change — the fleet snapshot and the discovery attrs carry the
agent's project JSON verbatim.

## What was verified, and how

- Three suites green: Go `./...`, the ansible docker-agent/module/system_monitor tests (275),
  frontend 2253.
- **Author canaries: 21 mutations, all killed.** That number predicted nothing — see below.
- **Four independent review lenses** (domain correctness, regression-on-callers, security/release
  surface, verification quality). Two HIGHs were found by *three* lenses independently.
- **A 33-mutation sweep by the verification lens: 12 survived (36%).** Every survivor is closed and
  **each one was re-run against the new test and killed** (15/15, including one whose anchor had
  moved and was re-targeted rather than written off).
- **The fix round was canaried too, and one fix was inert**: deleting the under-lock owner re-read
  left the whole suite green. The race test written for it then *also* passed against both
  implementations — it sequenced on a lock ref the test itself held, so the save was refused by a
  different code path. Waiting for a ref beyond the test's own makes it fail with `200`.

**Assumed, not measured:** nothing in this phase has run on a real host yet, unless §Rollout record
below says otherwise. In particular the first converge's compose-file relocation and container
relabel, and a Docker Jobs `update` against a relocated stack, are reasoned-about, not observed.

## Decisions, and the alternatives rejected

- **Ownership governs WRITES; the compose root governs READS.** Rejected: gating `/bundle` on
  editability (what the phase originally shipped). It broke nut's own converge — see Surprises.
- **`owner` is a pointer.** Rejected: a plain string. system_monitor's remediation and a cross-host
  copy re-register stacks knowing nothing about ownership; a flat `""` would unclaim them.
- **"Declare the same owner" rather than "refuse inline files".** Rejected: refusing all files,
  which locked out the only caller that legitimately writes them — the owner itself, which is how
  `deploy_nut_stack.yaml` converges.
- **One include, not four copies.** Six drifting copies of `upsert-agent-token.yml` is why.
- **No `ORIGINAL_<AGENT>_IMAGE`.** It is a retired-Portainer convention the agent never reads, and
  it is what put a bare image ID in esphome's `.env` (Phase 1 §Surprises).
- **The register retries 3× then FAILS.** Rejected: non-fatal. These deploys did not need a healthy
  agent before and now do — but an unregistered stack is not a visible degraded state, it is one
  Control Center cannot operate while every recap reads `ok`.
- **`project_not_editable` renamed.** Rejected: keeping the name for a refusal that now means
  "cannot read" — that is the conflation, living on in the wire vocabulary.

## Surprises

- 🔴 **Declaring the owner on nut broke nut.** `deploy_nut_stack.yaml` is a read-modify-write loop:
  it reads its own stack back through `GET /bundle` to preserve the `.env` before re-pushing the
  compose. `/bundle` answers a refusal with an **empty bundle, HTTP 200**, which that loop cannot
  tell from "no files". So from the first converge after ownership: `_nut_compose_changed` true
  forever, and `_nut_env_files` falling back to its hardcoded default — **the image pin overwritten
  on every run**, on a play Home Assistant fans out to all-nucs every 25 minutes. It is precisely
  the rollback the file's own header says it prevents, and it looks like ordinary work:
  `changed=true`, no error, nothing logged. **A capability flag that closes a read channel must be
  checked against every writer that reads its own state back.**
- 🔴 **The phase was dead on arrival and its own test did not notice.** The include read
  `docker_agent_data_dir`, defined in one vars file none of the four callers loads. Every deploy
  would have failed on the include's FIRST task. The test whose docstring claimed to check exactly
  this had a hand-written `REQUIRED_VARS` holding the six `owned_stack_*` names — structurally
  blind to the one var outside that family.
- **The frontend half shipped unreachable.** `editable` is false exactly when `owner` is set, so
  the owner-aware notice I added to the editor drawer could never render; the real surface is the
  disabled button, whose tooltip still said the files were outside the agent's mount. Three lenses
  found it independently; the two tests agreed with each other and proved the gap rather than
  catching it.
- **The security rationale I wrote for `owner` was false.** The bearer tokens stay in the legacy
  directories, outside the agent's mount entirely — the agent's container cannot read them at all —
  and the four templates carry no inline credentials. Ownership was never the secret boundary.
- **A mechanical fixture rename broke the property a test existed for**: `Gdrive-Agent` /
  `gdrive-agent` was a case-folded PAIR; renaming one side left `Gdrive-Agent` / `vendor-stack`,
  which tests something much weaker.

## Deferred

| Item | Why deferred | Measurement / trigger |
|---|---|---|
| **Ownership has no structural recovery.** `Owner` lives only in `projects.json`. If that file is lost while the containers run, `enrichFromLive` re-adopts them from container labels with NO owner → under the root → `managed:true`, and the editor reopens on Ansible's files until the next converge. | The obvious fix — a compose label the agent reads back — is a SECOND source of truth for one fact, which is its own failure mode. The recovery path exists (any agent-role run re-establishes it) and the trigger is narrow: the ComposeRoot must be wiped *while the containers keep running* (a host rebuild leaves nothing to adopt). | Reproduce by deleting `projects.json` and restarting the agent with the stacks up. Decide between the label and making the loss loud. **This is the next phase's first step.** |
| `deregister` takes no lock and makes no owner check | Subsumed by the row above: with structural recovery, a deregister cannot launder ownership | API-only; nothing in the UI calls it |
| Credentials inline in the traefik compose files | Belongs to the traefik role; user decision | carried over from Phase 1 |
| `rewrite_stack_container_dns.yaml` silently no-ops on an owned stack | Its `dns_pinning_stacks` is `[traefik, homeassistant]`, neither owned | becomes real the day traefik is declared owned — which this phase's reasoning invites |
| system-monitor CI job failures; two router tests red on main | pre-existing, unrelated | fail identically on origin/main |
| `test_docker_redeploy_network_stacks.py::test_read_engine…` fails only in a full-suite run | pre-existing — **verified failing identically on `main`** | passes when run alone |

## Next phase — first concrete step

Reproduce the ownership-loss case: on a throwaway host, with the four stacks running, `rm` the
agent's `projects.json`, restart the agent, and read `GET /v1/projects`. Expect `managed:true` and
no `owner` on all four. Then decide between (a) a compose label `projectEntryFromLive` reads back,
and (b) making the loss loud rather than silent (readiness reports "adopted entries under the root
with no recorded owner"). Write down which, and why the other was rejected.

## Traps

- 🔴 **Deploy order is the INVERSE of Phase 1's.** Phase 1's rule was "agents LAST", because the new
  agent's `replace` requirement broke old consumers. Here the dependency runs the other way: if the
  ansible roles converge before the new agent ships, the stacks register under the compose root
  with **no owner recorded** (an old agent ignores the unknown field), so `managed:true` and the UI
  offers Edit on Ansible's files — the exact bug this phase prevents, live in the gap. So:
  **docker-agent FIRST, ansible LAST.** Do not inherit "agents last" as a standing rule; re-derive
  it per phase.
- **`/bundle` answers a refusal with an empty bundle and HTTP 200.** Any consumer that re-pushes
  what it reads must not treat that as "no files". This bit nut; `rewrite_stack_container_dns.yaml`
  has the same shape and is inert only by luck.
- **Never re-render an owned stack's `.env`.** It is where the agent's `update` writes. Seed it.
- **A path-only register needs the compose file ON DISK first** (`400 compose_files_missing`), so
  the template task must precede the register. There is a test for the order.
- **The owner must be re-read under the lock** before any file write. The pre-lock snapshot is
  stale by construction — the handler does a Docker list and may wait 10s for the lock.
- **Do not add an `owner` branch to `StackEditorDrawer`.** It is unreachable; the wording lives on
  `ExternalStackEditButton`. There is a comment saying so in both files.
- **`docker_agent_data_dir` is defined in exactly one file.** The include loads it itself — do not
  "simplify" that into a bare `{{ docker_agent_data_dir }}`.
