# Agent stack ops — Phase 1 handoff: the agent refuses what it cannot do, and says so

Plan: `docs/AGENT_STACK_OPS_PLAN.md` (this repo). Companion handoff for the control-center
half: `control-center/docs/DOCKER_OPS_CAPABILITY_HANDOFF.md`.

## Status

| | docker-agent | control-center | ansible |
|---|---|---|---|
| Branch | `feat/ops-capability` | `feat/docker-ops-capability` | `feat/docker-hosts-default` |
| Final branch SHA | `140e393` | `a7151b93` → merged `9f638fc6` | see its handoff / commit log |
| Released / deployed | agents deploy LAST — see §Rollout record | `4.0.780`, deployed kd + ng, verified live | released before the agents |
| Review | 5 lens rounds + 4 real-data verifications on the fleet's 48 stacks / 60 services | 4 rounds | 4 rounds |

Merged is not deployed. If §Rollout record is missing below, this branch was merged but the agents were
NOT yet rolled out.

## Why this phase exists

2026-09-16: a Docker Jobs **update** of the `docker-agent` stack on kd-vps01 failed in 68 ms:
`load project: open /docker_container_volumes/docker-agent/docker-compose.yml: no such file or directory`.
Not the registry (the VPS pulls from `container-registry-api.kdhomeapps01.com:1443`, correct).
Four stacked causes:

1. The agent can only read compose files under `COMPOSE_ROOT` (`…/docker-agent/data`); Ansible
   renders its own compose and five other agent stacks' composes outside it. The UI and the
   Update Stack action offered ops the agent could never run — and for the agent's own stack,
   success would have been worse: compose stops the container running the job.
2. `ComposeProject.Managed` was `json:"managed,omitempty"`, so `false` never reached the wire and
   every `managed === false` guard in the UI was dead code.
3. Update Stack keyed only on `image_status=outdated`.
4. The VPS was stale because `docker-agent/deploy.yml` defaulted to `all_docker_hosts` — a group
   that never existed (0 hosts, exit 0) — and `-e target_servers=kdhome` has no VPS.

Decisions taken with the user: docker-agent itself stays **Ansible-only**; the other Ansible-owned
agent stacks become **updatable from Docker Jobs** (Phase 2).

## What shipped

### docker-agent — one capability decision, advertised and enforced

- **Per-op capability** (`capability.go`, the only source): invalid name → nothing; the agent's own
  stack → nothing; valid → `restart`, `down` (compose rebuilds these from labels); working dir under
  ComposeRoot → `up`, `pull`, `recreate`, `update`, editable; the control-path stack (the one owning
  the container named `TRAEFIK_DOCKER_DNS`) loses `down` only.
- **Wire:** projects carry `allowed_ops` (sorted, `[]` never null), `operable`, `managed`,
  `ops_blocked` (`self` > `invalid_name` > `outside_compose_root` > `control_path`); containers carry
  `self` / `control_path`. `GET /v1/projects` carries the same four fields.
- **Self identity:** container ID from `/proc/self/mountinfo` (only Docker's own setup mounts are
  trusted); project, name, `network_mode: container:<self>` dependents and the control path come
  from the container **list**, published atomically. Every mutating request takes a fresh bounded
  list; if it fails → 503 `self_identity_unavailable`. No lock is held across Docker I/O.
- **Container verbs** resolve every target with the daemon (`inspect`), dedupe by full ID, and the
  job acts on the checked full IDs — never on the raw reference (the `db`-named container vs an
  agent ID starting `db…` TOCTOU).
- **Every deterministic 4xx carries a `code`** (contract test fails a code-less 4xx). Only
  `idempotency_key_in_flight` is `"retryable": true`. Code table in `README.md` → API consumers.
- **`replace: true`** is required to register onto an existing project (registry or live label) →
  otherwise 409 `project_exists`.
- **Idempotency-Key** on every job-starting route: durable claim in events.sqlite before side
  effects; replay returns the original status + body (`Idempotency-Replayed: true`); different body
  → 422; concurrent → 409 retryable; 5xx not recorded; acted-but-unrecorded answers served from
  memory and written in the background, and a final bounded write in `close()`. Bounded by 24 h,
  5,000 rows **and** 2 MiB of bodies (measured worst case 5.1 MiB DB).
- **Body limit** 1 MiB on every body-carrying route (largest real register body: 12,047 B).
- **Compose load confinement:** one path resolver (compose + env files absolutised against the
  working dir, the same list handed to compose); explicit config paths so default/override discovery
  and the parent-dir walk never run; a model-only pre-scan confines `include:` (every form, env_file,
  project_directory), `extends.file` (exact base per reference), `label_file`, before any read; the
  load guard enforces the scan's approved set and fails closed.
- **Image tag writes** (every `update`/override edits a live compose or `.env` file): one token in place
  where compose actually reads the image (last defining compose document, last declaring env file,
  `.env` values that are templates followed through a single trailing tag variable, a shared undeclared
  tag variable declared above its FIRST use); every write verified by reloading the project (target
  moves, nothing else changes) or bytes restored; revert restores the exact original bytes; anything
  that can't be written that way is REFUSED with zero changes — never re-encoded. `validImageRef`
  agrees with `distribution/reference` and refuses image IDs.
- **Per-project serialisation:** every mutating job and register/copy holds the project's lock, keyed
  by the resolved working dir (two jobs tore `.env` 367 times in 2,000 before this). Register/copy
  wait ≤10 s then `409 project_busy` (`retryable: true`, not recorded under the key). Two registry
  entries can never share a directory (`409 working_dir_in_use`).
- **Service-targeted ops:** `"services": [...]` on up/recreate/pull/restart/down, narrowed exactly (no
  deps, no orphans); restart/down narrowed from container labels so they work outside the compose root;
  `service_ops` advertised per project; narrowed jobs recorded with `services` (job, event, `target`).
- **Unloadable projects** are refused at op time: `409 project_load_failed`, no job.

### ansible

- Real per-home host groups (`kdhome-docker-hosts` = kdhome + all-vps; `nghome-docker-hosts`),
  conditional host defaults for all five agent playbooks, a leading localhost play asserting
  `server_home`/target, two completed one-shot migrations deleted.
- `docker_agent_client.py`: Idempotency-Key per logical POST; a retry never duplicates work;
  in-flight polling with a deadline; coded answers are final; `register_project(replace=…)`
  required; transport errors never empty strings.
- system_monitor never force-removes containers the agent will not recreate (capability check
  first); a removal it made is persisted and the incident stays open until the container is seen
  again; an unreadable Engine read is unknown, not empty; sustained unknown → one operator notice.
- `docker_compose_members` (one helper): compose membership by labels, stale-stopped rule shared by
  the redeploy module and the monitor. The bond-migration recovery playbook now actually works.

### control-center

See its handoff. In one line each: `shared/refusal` (a 4xx is final only when it carries a
non-retryable `code`), dead letters visible on `/maintenance` + Overview (schema 19), peer health via
`/api/sync/status`, `shared/imagestatus`, discovery attribute filters + `name_exact` (the
cert-manager wrong-image bug), node page and Update Stack driven by `allowed_ops`.

## What was verified, and how

- Each repo's suite under `-race` / full vitest / targeted pytest; CI jobs reproduced locally in
  clean venvs because **GitHub Actions is billing-blocked** (runs don't start).
- Every review finding got a test, and every fix was canaried (re-introduce → a named test red).
  Author canaries and lens mutations are both reported in the commit messages; the lens numbers are
  the ones to trust.
- **Real corpus (4 passes):** every registered fleet bundle (10 agents, 78 entries, 48 loadable stacks,
  60 services) pulled through the router into scratch copies and driven through the real engine path
  under the production agent environment. Final pass at bbd2d1f: 240 writes → 222 in place with
  byte-exact reverts, 18 designed refusals (coupled immich/zwave shapes), 0 failures; `service_ops`
  truthful 78/78; every 4xx coded, no 5xx. Earlier passes found the H1 reformat, H2 torn `.env`,
  and the immich above-first-use split — all fixed and re-verified.
- **Live smoke** on kd-nuc01 with throwaway projects: compose ops, update, update-rollback restoring a
  messy compose file byte-for-byte, and service ops (recreate `a` never starts an operator-stopped `b`).
- Live read-only probes: Traefik answers text 404 for a route with no ready endpoints (why a bare
  status can never mean "refused"); the container LIST returns `Aliases: null` (why the control path
  is matched by exact name only).

## Decisions and their reasons

- **Refusal is a marker, not a status.** Rejected: "4xx except 404" — a hand-kept infra status list
  breaks on the next middleware. Measured: an infra 404 dead-lettered queued actions on first
  attempt in a round-2 build.
- **`replace` guards intent, not deploy.** Rejected: "relocation cannot deploy in the same request"
  (round 2) — it blocked nothing (register then up) and broke Ansible's single-request register.
- **Self from the container list, not a throttled inspect.** Rejected (round 1): a 30 s retry
  window that failed open and held a mutex across inspect, blocking `/health/ready`.
- **Idempotency stays in docker-agent, not agent-kit-go (yet).** Only docker-agent has a caller that
  resends POSTs. Promote it the day a second agent gets one (hard rule).
- **Deploy order: consumers first, agents last.** New consumers are backward compatible with old
  agents (gin ignores unknown JSON fields; no old handler reads headers — verified at c6d96e6); the
  new agent's `replace` requirement is not backward compatible with old consumers.

## Surprises

- **esphome's `CURRENT_ESPHOME_IMAGE` is a bare image ID on kd-nuc01 and ng-nuc01.** Not an agent
  write: the retired Portainer rollback (`portainer/manage_portainer_stack_update.yaml`) put the ID in
  the stack env and `migrate-portainer-composes.yml` copied it into `.env`. Correct value
  `esphome/esphome:stable` (its `ORIGINAL_ESPHOME_IMAGE`). Recovery needs one explicit op per host
  (awaiting the user's OK — it is a real version jump):
  `POST /v1/projects/esphome/op {"op":"update","override_image":"esphome/esphome:stable","override_service":"esphome"}`.
- **The deployed agent (0.1.17) already had the tag-write reformat and the unserialised-job tear** —
  the real-data verification found them in code this phase was rewriting, not in new code.

- The redeploy playbook's Step 1 could never have worked (compose passed as a string → TypeError).
- `handleGetQueue` silently dropped every queued action whose `error_message` was NULL.
- system_monitor's failed network listing read as "networks deleted" and stripped live Traefik refs.
- compose-go resolves relative `env_files` against the **process cwd** (`/` in the image) — every
  relative env file loaded the wrong file.
- Review fixes carried their own defects in every round (e.g. round-2's 404 dead-lettering, round-3's
  nested-include false refusal). Budget a re-review for any fix pass.

## Deferred register

| Item | Why deferred | Measurement |
|---|---|---|
| Job-history retention across agents (`compose_jobs`, duplicacy/gdrive/filemesh job tables) and controller (`docker_jobs`, `duplicacy_jobs`) | pre-existing, fleet-wide (5 repos); raised with the user as its own phase | no DELETE path exists in any of them; only `build_jobs` has retention |
| Idempotency → agent-kit-go | no second agent has a resending caller | — |
| system-monitor CI job failures unrelated to this work | pre-existing (cryptography/discord/systemd missing from requirements; gitignored pysyncobj; stuck_intent_reconciler) | 95 failed / 51 errors after this branch vs 97 / 489 before |
| Two router tests failing on main | not this work (price-monitor providerset DefaultClient; push-api roots/establish route) | fail identically on origin/main |
| Dead `duplicacy` project registered on 6 hosts | cleanup, needs user OK | 0 containers |
| `homeassistant` registry entries on kd-nuc02 / kd-pi01 / kd-vm01 point at a leftover traefik compose; kd-nuc01 `myapp` has a 1-byte compose | registry junk from migration; deregister needs user OK | 0 homeassistant containers on those hosts |
| Inline credentials in the traefik compose files (served verbatim by `/bundle`) | belongs to the traefik role; move to env from Bitwarden; user decision | kd-nuc01 traefik compose carries a Cloudflare token + AWS keys |
| esphome image-ID pin on kd-nuc01 / ng-nuc01 | recovery op needs user OK | see Surprises |

## Next phase — first concrete step

Phase 2 (Ansible-owned agent stacks updatable): add `ProjectEntry.Owner` + `"owner"` on register
(editable = … && Owner == ""), then relocate **gdrive-agent on one host** first: template its compose
to `{{ docker_agent_data_dir }}/gdrive-agent/docker-compose.yml`, write the image pin to that dir's
`.env`, `compose up` from the new path, register path-only with `replace: true, owner: ansible`.
Verify: `allowed_ops` gains `update`, `managed` stays false, a Docker Jobs update completes with
health check, and a re-run of the role is idempotent.

## Traps

- **Deploy agents LAST.** An agent deployed before the consumers makes every old re-register 409.
- **A 4xx without `code` is retried ~100x cross-site.** Any new deterministic refusal in the agent
  must carry a code — the contract test will fail otherwise; don't weaken it.
- **`retryable: true` is allowlisted.** Adding a transient 4xx means adding it to the allowlist AND
  checking control-center's `shared/refusal` tests.
- **Control-path protection depends on `TRAEFIK_DOCKER_DNS` being set** and equal to the Traefik
  container's exact name on every host (readiness reports `control_path_error` otherwise).
- **Mountinfo failure (not Docker) does not gate ops** — deliberate; readiness reports it.
- **Never widen confinement to "any base"** — M1 showed it leaks an outside `extends.file`.
- **Never reintroduce a YAML re-encode fallback** in tag writes — it reformatted real compose files
  permanently on a failed override's revert.
- **A consumer must check `service_ops` before sending `services`** — an older agent ignores the field
  and acts on the whole project.
- **`allowed_ops` is structural** (never loads files); load failures surface only at op time.
