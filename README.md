# docker-agent

A per-host Docker control-plane agent. One Go binary runs on each Docker host, talks to the
local Docker Engine over a mounted socket, and exposes an HTTP + WebSocket API for:

- **container lifecycle** — start / stop / restart / kill / remove, individually or in bounded
  bulk, plus live and historical container log streaming;
- **Docker Compose project management** — register, list, bundle, copy, deregister, and run
  `up` / `down` / `pull` / `restart` / `recreate` using the **Compose v5 SDK in-process**, as Go
  library calls over the same socket. There is no `docker compose` subprocess and no stdout
  scraping; there is no docker CLI, buildx or compose binary in the image at all;
- **image-update detection** — per container, "is a newer image available?", answered against
  OCI registries (manifest digests) and GitHub (releases, packages keyed off branch CI builds),
  with zero-config auto-detection from tag shape and OCI labels;
- **a stack-update engine** — resolve target images, snapshot for rollback, apply tags to the
  on-host compose/.env, pull, `up --force-recreate`, wait for containers to actually swap and
  become healthy, and **roll back to the exact pre-update image IDs** if they don't.

It pushes its state outward to a central controller: a live fleet snapshot, a full discovery
snapshot, and a durable event stream for every job it runs.

Honestly framed: this is an in-house alternative to Portainer plus a pile of update automation.
It exists because that combination — an agent for host access, a management UI holding stack
state, and separate scripts doing image checks and rolling updates — had three sources of truth
and no rollback. Here the on-host compose files stay the source of truth, the update is one
pipeline with a health gate, and the whole thing is a single ~9k-line binary you can read.

---

## ⚠ Security — read this before you deploy anything

**This agent has no inbound authentication of any kind.** Not a bearer token, not a shared
secret, not mTLS, not an IP allowlist. No handler on any of the 23 routes reads the
`Authorization` header — the bearer token this agent holds is *outbound only*, to authenticate
itself to the controller.

That matters because of what the agent can do:

- **It mounts `/var/run/docker.sock`.** Access to the Docker socket is root-equivalent on the
  host: anyone who can reach this API can start a privileged container that mounts `/`, and own
  the machine. This is the same posture as any Docker management agent, but it is worth stating
  plainly rather than implying.
- **It writes files on the host.** `POST /v1/projects` writes arbitrary file content under
  `$COMPOSE_ROOT` and can immediately `up` it.
- **The WebSocket upgrader accepts any `Origin`** (`fleet.go:80` —
  `CheckOrigin: func(*http.Request) bool { return true }`). A browser on any page can open
  `/ws/fleet` against a reachable agent.

The deployment this was built for puts the agent on a private container network with a reverse
proxy in front that terminates mTLS and is the only thing that can route to it. **That proxy is
the entire security boundary.** If you publish port `8080` to a host interface, or to the
internet, you are handing out host takeover.

If you adopt this, add authentication before you expose it. A middleware checking a shared
secret in front of `registerRoutes` is about ten lines, and there is no reason not to.

---

## Architecture

One flat `package main`. 29 source files plus 13 test files.

### Core and wiring

| File | Role |
|---|---|
| `main.go` | Startup order, the 23-route table, graceful shutdown. Binds `:8080`. |
| `app.go` | Builds and wires every subsystem; starts the background workers; the boot reconcile loop. |
| `config.go` | Env → `Config`. Reads the bearer token. `deriveNodeName` (see Configuration). |
| `types.go` | The `Job` model: states, events, the bounded log ring, live subscribers. |
| `jobs.go` | `jobRegistry` — owns in-flight jobs, drives the engine, fans lifecycle changes to the event hook + fleet hub, evicts terminal jobs. |
| `engine.go` | Operation names and dispatch; container-op and bulk fan-out execution. |
| `handlers.go` | Health, container-lifecycle and job HTTP handlers; the job-log WebSocket. |
| `logging_setup.go` | Optional rotating on-disk log file (lumberjack) teed alongside stderr. |

### Docker layer

| File | Role |
|---|---|
| `dockerclient.go` | The moby Engine API wrapper. The fleet snapshot is ONE `ContainerList` call (health and exit code parsed from the status string — no per-container inspect); lifecycle verbs; log stream demux; the inspects the update engine needs. |
| `networks.go` | Docker network listing and per-container network attachments, for the discovery snapshot. |
| `containerlogs.go` | Container log HTTP + WebSocket handlers (live follow and a bounded back-paging window). |
| `wslog.go` | The shared WebSocket line pump used by both job logs and container logs — backlog, coalesced live frames, keepalive ping, disconnect detection. |

### Jobs, events and durability

| File | Role |
|---|---|
| `events.go` | `events.sqlite`: the durable outbound event queue (kit `eventoutbox`) plus a local `compose_jobs` table giving the job list restart-survival. |
| `orphan_sweep.go` | At boot, *before* serving: any job left `running`/`pending` by a prior container exit is failed and a synthetic terminal event is enqueued, so the controller never keeps a zombie "running". |
| `heartbeat.go` | Writes `heartbeat.last` every 30s (and `0` on clean shutdown), so the next boot can log whether the gap was a restart or a crash. |

### Compose

| File | Role |
|---|---|
| `compose.go` | The in-process Compose v5 SDK binding. One `command.Cli` at startup; a fresh `api.Compose` per job so progress events and stream output land in that job's log ring. |
| `compose_handlers.go` | Project op / register / deregister / bundle / copy handlers; safe file writing under `$COMPOSE_ROOT`. |
| `projects.go` | The durable project registry (`projects.json`): atomic persistence, auto-adoption of already-running projects from compose labels, and `underComposeRoot` — the structural "does the agent own this stack's files?" predicate. |

### Update engine

| File | Role |
|---|---|
| `stackengine.go` | `updateProject` — the whole pipeline (below), plus the rollback baseline, per-service tag persistence, rollback, and the failed-container log dump. |
| `resolvers.go` | The four `ImageResolver` kinds and the selection logic; the upstream-compose fetch/cache/interpolate path. |
| `healthwait.go` | Two-stage wait: container-ID swap detection, then a health poll with crash-loop (restart-count) detection. |
| `tagwrite.go` | Writes resolved images into the project's own files: one token in place (the compose scalar where compose takes the image from, or the value of its `.env` assignment), verified by reloading the project, reverted byte for byte. Refuses — changing nothing — whatever cannot be written that way. |
| `envscan.go` | compose-go's dotenv parser with byte offsets kept, so an assignment can be rewritten in place — and a `KEY=` line inside another variable's quoted value is never mistaken for one. |
| `projectlock.go` | Per-project serialisation: every project job, and the register/copy writes, hold the project's lock. |
| `netreconcile.go` | Post-update check that no container came back attached to fewer networks than its service declares; one corrective `up --force-recreate` if so. Best-effort, never fails the update. |

### Image checking

| File | Role |
|---|---|
| `imagecheck.go` | The slow, jittered, capped, cached pass. Strategy resolution (`central override ?? auto-detect`), per-unique-image checks with bounded concurrency, the result cache, and the fleet/project rollup stamping. |
| `registry.go` | OCI v2 registry client: image-reference parsing, manifest-digest `HEAD` with bearer re-auth, Docker Hub normalisation, `~/.docker/config.json` credentials, a per-scope token cache, and the existence gate. |
| `github.go` | GitHub App installation-token vend (via the controller), a retrying/rate-limit-aware API client, semver comparison, latest-release lookup, and GitHub Packages tag resolution keyed off branch commit SHAs. |

### Outbound

| File | Role |
|---|---|
| `network.go` | The one pooled HTTP client for every controller call: bearer round-tripper plus the optional dial rewriter (see Controller contract). |
| `discovery.go` | The periodic full-snapshot discovery push, with a TTL-cached network gather and an out-of-band trigger after each image-check pass. |
| `fleet.go` | The fleet hub (kit `fleet.Hub`) — coalesced snapshot broadcast to `/ws/fleet` subscribers, with image status stamped on and registry-known projects merged in. |

Shared operational scaffolding comes from
[`agent-kit-go`](https://github.com/Daniel-dev22/agent-kit-go): `logging`, `fleet` (the coalescing
snapshot hub), `eventoutbox` (the durable delivery queue), `jobstore` (orphan sweep + reconcile
listing) and `reconcile`.

---

## HTTP API

Everything listens on `:8080`. All bodies are JSON, and every request body is capped at 1 MiB
(`413 {"code":"request_too_large"}`, refused before any handler reads it); the largest real one on
the fleet is a 12 KB register.

The async pattern is uniform: **any mutation returns `202 Accepted` with a `job_id`**, and you
observe the result on `/ws/jobs/:id/logs`, `GET /v1/jobs/:id`, or the next `/ws/fleet` snapshot.
Jobs run on a detached context, so the operation is not cancelled when the HTTP response is
written.

### Idempotency-Key

Every request that can start a job — `POST /v1/projects/:name/op`, `POST /v1/projects`,
`POST /v1/projects/:name/copy`, `POST /v1/containers/:id/{start,stop,restart}`,
`DELETE /v1/containers/:id` and `POST /v1/containers/bulk` — accepts an optional
**`Idempotency-Key`** header, so a caller that lost a response (the agent restarted mid-request, a
timeout, a dropped connection) can resend without running the operation twice.

- The key is 1–128 printable ASCII characters, one header line. Anything else is
  `400 {"code":"invalid_idempotency_key"}` and nothing runs.
- Scope is method + path + key. The agent records the answer it gave — status and JSON body —
  in `events.sqlite` under `CONFIG_DIR`, so it survives an agent restart.
- A resend with the same key and the same body returns the **original status and body verbatim**
  (a `202 {"job_id":…}` or the original refusal) and runs nothing. Replayed responses carry
  `Idempotency-Replayed: true`. "Same body" is compared on canonical JSON, so key order and
  whitespace do not matter.
- The same key with a different body: `422 {"code":"idempotency_key_reused"}`.
- The same key while the first request is still being handled: `409
  {"code":"idempotency_key_in_flight","retryable":true}` — the claim is atomic and taken before
  anything runs, so the duplicate never executes. Retry (poll) for the answer.
- Only deterministic answers are recorded (2xx and 4xx). A 5xx — notably `503
  self_identity_unavailable` — releases the key, so a retry can succeed once the daemon answers.
- A claim still in flight when the agent died is released at boot: any job it had started was
  interrupted and failed by the orphan sweep, so a retry correctly runs again.
- Keys are honoured for 24 hours. The table is bounded by rows (5,000 answers) and by bytes (2 MiB
  of answers), oldest first; with both caps binding the whole database measured 5.1 MiB.
- If an answer cannot be stored after the request acted, it is served to retries from memory while
  it is written in the background. Choose a new key per logical operation, not per retry.

Without the header, every endpoint behaves exactly as described below.

### Health (2)

| Method | Path | Response |
|---|---|---|
| `GET` | `/health/live` | `200 {"status":"ok"}` |
| `GET` | `/health/ready` | `200 {"status":"ready", "self":{…}, "projects":{"shared_working_dirs":{…}}}` |

Neither touches the Docker daemon: readiness reports the last published self identity
(`container_id`, `name`, `projects`, `ambiguous`, `control_path` / `control_path_error`,
`observed_at`, `last_list_error`, or `error`) and never gates on it. The image's `HEALTHCHECK`
curls `/health/ready`. `projects.shared_working_dirs` maps a working directory (symlinks
resolved) to the registry entries that share it — `{}` when none do. A register refuses to
create one, but a `projects.json` from an earlier release can hold one; it is reported, not
gated, and changes through either name are still serialised (they take one lock).

### Jobs (4)

| Method | Path | Response |
|---|---|---|
| `GET` | `/v1/jobs` | `200` — array of job objects: in-flight jobs merged with recent persisted ones (in-memory wins on id collision), newest first. |
| `GET` | `/v1/jobs/:id` | `200` job object, falling back to the sqlite history. `404 {"error":"job not found"}`. |
| `GET` | `/v1/jobs/:id/log` | `200 {"id":…, "lines":[…]}` — the in-memory ring (last 2000 lines). `404` once the job has been evicted from memory. |
| `POST` | `/v1/jobs/:id/cancel` | `200 {"cancelled":true}`, or `409 {"error":"job not cancellable (unknown or already terminal)"}`. |

A job object is `{id, project?, operation, target?, state, started_at, completed_at?, exit_code,
error?, line_count, trigger_key?}` with `state` one of
`pending|running|completed|failed|cancelled`.

**One change per project at a time.** Every project-scoped job (`up`, `down`, `pull`, `restart`,
`recreate`, `update`) holds its project's lock for its whole run; a second one on the same
project waits, `running`, with `queued: waiting for job <id> (<op>) on project <name>` in its
log. The wait honours cancellation and gives up after `DOCKER_COMPOSE_OP_TIMEOUT`, then the op
runs with its own full timeout. Container verbs and jobs on other projects are not held up.
Without it, two updates on one stack each rewrote the same `.env`: measured against the real
writer, 2,000 concurrent pairs tore 367 files and lost 814 updates.

### Container lifecycle (6)

| Method | Path | Body | Response |
|---|---|---|---|
| `POST` | `/v1/containers/:id/start` | optional `{trigger_key?}` | `202 {"job_id":…, "state":"pending"}` |
| `POST` | `/v1/containers/:id/stop` | optional `{timeout?, trigger_key?}` | `202` |
| `POST` | `/v1/containers/:id/restart` | optional `{timeout?, trigger_key?}` | `202` |
| `DELETE` | `/v1/containers/:id` | optional `{force?, trigger_key?}` | `202` |
| `POST` | `/v1/containers/bulk` | `{action, ids[], force?, timeout?, trigger_key?}` | `202 {"job_id":…, "state":…, "count":N}`; `400` on an unknown action or empty `ids` |

Every container verb first refuses a protected target — `409 self_container`, `409
control_path_container`, `503 self_identity_unavailable` — see "What the agent will do with a
project, and why" under Mounts.
| `GET` | `/v1/containers/:id/logs` | — | `200 {"lines":[…]}` |

`timeout` is the SIGTERM grace in seconds (stop/restart); `force` kills a running container
before removing it. `action` is `start|stop|restart|kill|remove`. A bulk job fans out with
bounded concurrency (`DOCKER_BULK_CONCURRENCY`) and fails if **any** container op fails, with a
per-container result line in the job log either way.

`GET /v1/containers/:id/logs` is the non-following history window for a viewer paging backwards:
`?tail=` (default 500), `?since=`, `?until=<oldest-timestamp-you-have>`. Lines carry RFC3339
timestamps. It **degrades to `200 {"lines":[], "error":"…"}`** rather than a 500, so a proxy in
front never relays a scary error and a viewer simply stops paging.

### Compose projects (6)

| Method | Path | Body | Response |
|---|---|---|---|
| `GET` | `/v1/projects` | — | `200 {"projects":[…]}` — the durable registry, sorted by name, each entry with `allowed_ops`, `operable`, `managed`, `ops_blocked`. |
| `POST` | `/v1/projects` | see below | `200 {"registered":name, "job_id"?:…}` |
| `DELETE` | `/v1/projects/:name` | — | `200 {"deregistered":name}` |
| `POST` | `/v1/projects/:name/op` | `{op, services?, timeout?, override_image?, override_service?, trigger_key?}` | `202 {"job_id":…, "state":…}` |
| `GET` | `/v1/projects/:name/bundle` | — | `200 {name, working_dir, compose_files:[{name,content}], env_files:[…]}` |
| `POST` | `/v1/projects/:name/copy` | `{new_name, deploy?}` | `200 {"copied":…, "working_dir":…, "job_id"?:…}` |

**Register** takes either an existing on-host directory —
`{name, working_dir, compose_files?, profiles?, env_files?}` — or inline content:
`{name, files:{"<relpath>":"<content>"}, deploy:true}`, which writes the files under
`$COMPOSE_ROOT/<name>/` (rejecting any path escaping that directory) and optionally `up`s them
immediately. The inline form is how a stack is copied to a different host: `GET` the source's
bundle, `POST` it to the target agent.

Registering onto a project that **already exists** — registered, or running as containers labelled
with that project — requires `"replace": true`, whatever the directory or `deploy`; otherwise it is
`409 project_exists` carrying the existing `working_dir`. This is accident protection (a copy or a
typo re-pointing someone else's stack), not a security boundary: the API is unauthenticated
in-network. With `replace:true`, re-pointing and deploying in one request is allowed, and the
deploy still goes through the capability gate for the new entry. Ansible and the editor send
`replace:true`; a cross-host copy does not.

Every refusal happens before any file is written; the codes are in the table under Mounts. A write
that fails after validation is a `500` (transient).

**Op** accepts `up | down | pull | restart | recreate | update`.

**`services`** (optional, `up`, `recreate`, `pull`, `restart`, `down`) narrows the op to exactly
those services; absent or empty is the whole project. Each must be a compose service name and a
service of the project as it loads now, else `400 unknown_service` naming it. The list is a set:
order and duplicates do not matter, to the op or to the Idempotency-Key fingerprint. A narrowed op
never touches anything else: `up`/`recreate`/`pull` run with no dependencies (`--no-deps`) and
remove no orphans, so a dependency an operator stopped stays stopped; `restart` restarts only
them; `down` stops and removes only their containers — compose's own service-scoped `down` also
removes the services depending on them and tries to remove the project's networks, so it is not
used. Volumes, anonymous ones included, are kept. `update` does not take `services`; it targets
one service with `override_service`. `GET /v1/projects` entries carry `"service_ops": true` on an
agent that supports this.

It returns:
- `400` if `op` is not one of those,
- `503 {"error":"compose backend unavailable on this host"}` if the compose backend failed to
  initialise at startup (the agent deliberately keeps running in that case — the read-only fleet
  and container lifecycle still work),
- `404` if the project is neither in the registry nor currently running,
- `409 project_not_operable` / `self_project` if the op is not in the project's `allowed_ops`
  (checked before the `503`: retrying cannot fix a refusal),
- `503 self_identity_unavailable` if the container list needed to decide that failed,
- `202` otherwise.

`down` never removes volumes. `update` runs the stack-update engine (next section).

**Deregister** removes the project from the index only. It does not touch the on-host files or
the running containers.

**Copy** duplicates a project on the *same* host under a new name, returning `409` if that name
already exists, `404` if the source is unknown, `400 invalid_project_name`, and `409
project_not_editable` / `self_project` when the source cannot be read or either name is the
agent's own.

Bundle reads are deliberately forgiving: a compose file the agent cannot read (an
externally-provisioned stack living outside `$COMPOSE_ROOT`) is skipped with a warning, and the
response still carries `working_dir` so a UI can say "not editable here, files live at …"
instead of showing an error. Such projects are also flagged `managed:false` in the fleet
snapshot up front.

### Image checks (2)

| Method | Path | Response |
|---|---|---|
| `GET` | `/v1/images/checks` | `200 {"checks":[…]}` — the raw result cache (debugging; the live status normally rides `/ws/fleet`). |
| `POST` | `/v1/images/refresh` | `202 {"triggered":true}`, or `503` if the checker isn't running. |

`refresh` requests a pass that **bypasses the TTL coalesce**, so a strategy you just changed is
recomputed within seconds instead of up to one TTL later — but admits at most one bypass per
`DOCKER_IMAGECHECK_FORCE_DEBOUNCE` window, so a spammed refresh button collapses to one real pass.

### WebSockets (3)

| Path | Payload |
|---|---|
| `/ws/jobs/:id/logs` | Newline-delimited text frames: the job's log backlog, then live lines. An unknown job gets one text frame, `job not found or no live log`. |
| `/ws/containers/:id/logs` | Newline-delimited text frames, live tail with `?tail=`/`?since=`. The Docker stream is opened **before** the upgrade, so a missing container is a clean `404` JSON response instead of a post-upgrade error. |
| `/ws/fleet` | JSON `{"type":"snapshot","data":{host, containers[], compose_projects[]}}` — one immediately on connect, then coalesced snapshots whenever anything changes. |

All three write coalesced frames (a burst of N queued log lines becomes one frame) and send a
20-second server ping. There is no client heartbeat.

---

## The update engine

`POST /v1/projects/<name>/op {"op":"update"}` runs one pipeline (`stackengine.go:updateProject`):

1. **Load + baseline.** Parse the project's compose files, then snapshot every current
   container: name, id, service, and image ID. That snapshot is the rollback anchor — rollback
   pins to image **IDs**, so it is correct even for moving tags where the tag itself did not
   change.
2. **Resolve.** Run the project's resolver as a read-only dry-run to get
   `{service: target-image}`. Failures here are **loud**: a service that is outdated but whose
   target cannot be resolved fails the update rather than silently redeploying the old pin and
   reporting success. For non-`registry` resolvers, an empty plan means *up to date* and the
   update stops cleanly — no needless force-recreate.
3. **Persist.** Write each target into the project's files (`tagwrite.go`), then deploy the
   project **as it now loads from disk** — so what runs is exactly what the next `compose up`
   reads. The write:
   - changes one token per service and nothing else: a literal image where compose takes it
     from (the last compose file, and the last YAML document in it, that sets it); a
     `${VAR}` image as the value of VAR's assignment in the env file compose takes it from,
     keeping its quotes, inline comment and line ending — or a new `VAR=` line when nothing
     declares it; a template whose only variable part is a trailing tag (`reg/app:${TAG}`,
     `…:${TAG:-release}`) as the value of that tag variable;
   - refuses, changing nothing, whatever cannot be written that way: an anchored or aliased
     image, a tagged, block or escaped scalar, a merge key, a template that varies anything
     but a trailing tag, a variable declared by a bare inheriting line or set in the agent's
     own environment, a spelling that would not read back as the image;
   - is **verified**: the project is reloaded with the agent's loader, every target must
     resolve to its new image, and no other service, variable use, network or volume may
     differ — a later assignment that shadows the write, or another service that shares the
     variable, puts the original bytes back and fails the update.

   Running state never gets ahead of disk: if persistence fails, nothing was written.
4. **Pull, then `up --force-recreate`.** Both in-process, with progress streaming into the job
   log. A pull failure reverts the written files to their exact prior bytes (nothing has been
   recreated yet).
5. **Health-wait** (`healthwait.go`), in two stages:
   - *Swap detection* — wait for container IDs to change. Partial-update tolerant: it succeeds
     as soon as all containers have swapped, or once one has swapped plus a 20s grace for the
     rest. Containers that vanish mid-recreation are expected. Zero swaps at timeout is a
     failure — the recreate did not take.
   - *Health poll* — wait for every monitored container to reach `healthy` or `none`, failing
     fast on a **crash loop** (restart count climbing more than 3 past its baseline) or a
     stopped container. Containers with no shell (so no healthcheck can ever report) are
     excluded — see `DOCKER_HEALTH_EXCLUDES`.
6. **Rollback on failure.** Dump the tail of each unhealthy container's logs into the job log,
   revert the persisted tags for the affected services — each file rebuilt from its original
   bytes with only the kept services' edits, never by writing an old value back; a file changed
   by hand since the update is left alone and reported — redeploy them pinned to their
   pre-update image IDs with `PullPolicy: never`, and re-health-wait to report whether the
   rollback itself came back healthy. Scope is `per-container` by default (only the unhealthy
   services roll back; healthy updated ones keep their new version) or `whole-stack` for a
   version-locked project where a mixed set would be incoherent. A **fresh deploy** (the stack
   was down, so there is no prior version) has nothing to roll back to and says so.
7. **Network reconcile** (`netreconcile.go`). If any container came back attached to fewer
   networks than its compose service declares — which happens when a shared external network is
   itself recreated during the deploy — issue one corrective `up --force-recreate`.
   Best-effort; it never fails an update that already passed its health gate.

### The four resolver kinds

A resolver answers "what do I deploy?" and returns **only** the services whose image should
change. Services absent from the map keep their compose image and are still pulled — which is
why a moving tag needs no rewrite at all.

| Kind | What it does |
|---|---|
| `registry` | **The default.** Per service, reuse the image checker's own `checkUnit`, so the deploy target is exactly the status check's verified answer — auto-detection, GitHub sources, the registry-existence gate and image-scoped overrides all behave identically in both paths, with no second implementation to drift. A moving tag (`registry-digest`) yields no rewrite: `pull` fetches the new digest and `up` recreates. |
| `upstream-compose` | For a project whose services are version-locked to one upstream release. Resolves the repo's latest release tag, fetches that tag's `docker-compose.yml` (via the authenticated GitHub Contents API), interpolates its `${VAR:-default}` image tags, and maps upstream service names onto local ones via `service_map`. Services intentionally on a moving tag are skipped — they auto-track upstream already, so only concretely-pinned services drive the stack's status. Bodies are cached by resolved URL (a release tag's compose file is immutable), collapsing the periodic pass, the update's re-resolve and the post-update recheck into one fetch per release. |
| `build-agent` | **A stub.** For a stack whose image is *built* rather than pulled. Triggering that build needs the stack's build descriptor and repo/ref, which is not wired here, so `plan` returns an error naming the resolver rather than pretending to succeed. Detection still works for such a stack if its image carries the OCI `source`/`version` labels. |
| `override` | Deploy one explicit pinned image. Used both by a central strategy row and by the request-level `override_image` (the manual "deploy exactly this tag" path), which wins over everything else. The target service comes from `override_service`, else the service named like the project, else the sole service. Already on that image ⇒ empty plan, so detection reports *updated* and the engine skips a pointless recreate. |

Selection is `request override_image` → project-scoped central override's `image_resolver` (or
one derived from its `version_source`) → `registry`.

### How "outdated" is decided

Separately from the update, a background pass (`imagecheck.go`) answers "is something newer?"
per unique `(project, service, image)`. It is deliberately **slow, jittered, capped and cached**
and never runs inline in the fleet snapshot. The effective strategy per image is
`central override ?? auto-detected`:

- **auto-detect** from the image itself — a fully pinned `X.Y.Z` tag with an OCI `source` label
  means "track that repo's GitHub releases"; anything else (`X`, `X.Y`, `latest`, `stable`,
  `edge`, a date) is a moving tag, so digest drift on the same tag is the signal;
- **central override** — a strategy row fetched from the controller
  (`GET /api/docker/strategies`) for the cases that cannot be inferred.

Repo-driven candidates must pass a **registry-existence gate**: never propose a tag whose image
has not actually been pushed. For a coupled (non-`registry`) project, the *status* is derived
from the resolver's dry-run — so "is it outdated?" and "what would I deploy?" are the same
answer and cannot disagree, and a coupled dependency with its own newer upstream does not flag
the whole stack.

---

## Configuration

Everything is environment variables. Only two are required.

| Variable | Default | Meaning |
|---|---|---|
| `SITE_ID` | **required** | Deployment site identifier (e.g. `site-a`). Also the required hostname prefix — see below. |
| `CONTROL_CENTER_URL` | **required** | Base URL of the controller, e.g. `https://controller.example.com:1443`. |
| `BEARER_TOKEN_FILE` | `/etc/docker-agent/bearer-token` | File holding this node's bearer token. **The agent exits if it is missing or empty.** |
| `CONFIG_DIR` | `/var/lib/docker-agent` | Persistent state: `events.sqlite` and `heartbeat.last`. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker Engine endpoint. |
| `COMPOSE_ROOT` | `/srv/containers/docker-agent/data` | Agent-owned compose data dir. Also the "is this project managed/editable?" predicate. See Mounts. |
| `COMPOSE_REGISTRY_PATH` | `$COMPOSE_ROOT/projects.json` | The durable project index. |
| `REGISTRY_AUTH_FILE` | *(unset)* | Path to a mounted `~/.docker/config.json` for private-registry digest checks and pulls. |
| `TRAEFIK_DOCKER_DNS` | *(empty)* | If set, outbound controller calls dial **this** name instead of the URL host (see Controller contract). Empty = dial the URL host directly. |
| `TRAEFIK_DIAL_PORT` | `1443` | Port used with `TRAEFIK_DOCKER_DNS`. |
| `DOCKER_PROXY_CONTAINER` | `traefik` | Name/id of the host's reverse-proxy container whose network attachments are published in the discovery snapshot. |
| `DOCKER_BULK_CONCURRENCY` | `GOMAXPROCS` clamped to `[2,4]` | Max simultaneous container ops in a bulk fan-out, and the inspect concurrency during a health-wait. |
| `DOCKER_COMPOSE_OP_TIMEOUT` | `30m` | Hard bound on one compose op or update, so a wedged pull can't run forever. |
| `DOCKER_HEALTH_SWAP_TIMEOUT` | `120s` (`300s` on a slow host class) | Update: how long to wait for container IDs to change. |
| `DOCKER_HEALTH_TIMEOUT` | `180s` (`360s` on a slow host class) | Update: how long to wait for containers to become healthy. |
| `DOCKER_SLOW_HOST_CLASSES` | `pi` | Comma-separated node-name classes that get the longer health budget. A node name is `<class><ordinal>` (`pi01`, `nuc02`), so the class is the name minus trailing digits. Set to the empty string for no slow classes. |
| `DOCKER_HEALTH_EXCLUDES` | `portainer,adguardhome-sync` | Comma-separated container names skipped in the health phase, because their images ship no shell and a compose healthcheck can therefore never report anything. Set to the empty string to exclude nothing. Per-project strategy overrides take precedence. |
| `DOCKER_IMAGECHECK_INTERVAL` | `15m` | Period of the image-outdated pass (jittered per node). |
| `DOCKER_IMAGECHECK_TTL` | `5m` | Minimum gap between passes; rapid triggers coalesce. |
| `DOCKER_IMAGECHECK_FORCE_DEBOUNCE` | `15s` | Minimum gap between TTL-bypassing forces admitted by `POST /v1/images/refresh`. |
| `DOCKER_IMAGECHECK_CONCURRENCY` | same as bulk default | Max concurrent registry/GitHub lookups in one pass. |
| `DOCKER_IMAGE_ARCH` | `amd64` | Architecture suffix used when matching architecture-tagged image candidates. |
| `DOCKER_REGISTRY_INSECURE` | *(empty)* | Comma-separated registry hosts whose TLS certificate is not verified (self-signed internal registries). |
| `DOCKER_DISCOVERY_INTERVAL` | `5m` | Period of the discovery snapshot push (jittered per node). |
| `DOCKER_NETWORK_TTL` | `120s` | TTL of the cached network gather, so a periodic push and a triggered one don't both hit the socket. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LOG_DIR` | *(unset)* | If set, `slog` is also written to a rotating `<LOG_DIR>/agent.log` (20 MB × 5, 14 days, gzipped) alongside stderr. |
| `LOGUTIL_FORMAT` | `json` | Set to `text` for a human-readable handler. Only consulted when `LOG_DIR` is set. |

### The non-obvious hard requirement: the container hostname

`deriveNodeName` (`config.go`) takes this agent's node identity from the **container hostname**,
which must be exactly `<SITE_ID>-<node>`:

```
SITE_ID=site-a, hostname=site-a-pi01   →   NodeName = "pi01"
```

There is no `NODE_NAME` variable, on purpose: the hostname already encodes it, and this identity
must match the name the rest of the system uses for the same host. If the hostname is
unreadable, does not start with `<SITE_ID>-`, or leaves an empty remainder, the agent **logs an
error and `os.Exit(1)`** rather than start up reporting a wrong or ambiguous node name.

In compose that is `hostname: site-a-pi01`; in Kubernetes it comes from the pod name.

---

## Mounts

| Mount | Mode | Why |
|---|---|---|
| `/var/run/docker.sock` | **rw** | Everything. Root-equivalent on the host — see the security section. |
| `$COMPOSE_ROOT` | **rw** | Compose project files the agent owns. **Must be bind-mounted at an IDENTICAL host and container path.** |
| `$CONFIG_DIR` | **rw** | `events.sqlite` (the durable event outbox + local job history) and `heartbeat.last`. Losing it means undelivered events are lost and the job list starts empty. |
| `$BEARER_TOKEN_FILE` | ro | The node's outbound identity. Mode `0600` on the host. |
| `$REGISTRY_AUTH_FILE` | ro | Optional. `~/.docker/config.json` for private registries. |
| `$LOG_DIR` | rw | Optional, only if you want the rotating on-disk log. |

**Why `$COMPOSE_ROOT` must be the same path inside and out.** Docker Compose records
host-resolved absolute paths — in the `com.docker.compose.project.working_dir` and
`config_files` labels it writes onto every container, and in the bind-mount sources it hands the
daemon. The agent runs compose *in-process*, so those paths are resolved from the agent's own
filesystem view. If the container saw `/data` where the host has `/srv/containers/agent/data`,
compose would ask the daemon to bind-mount `/data/...` — which does not exist on the host — and
the labels written would point at nothing. Mount it 1:1 (`-v /srv/x:/srv/x`) and the problem
disappears.

That same path doubles as an **ownership predicate**. `underComposeRoot` (`projects.go`) asks a
purely structural question: is this project's working directory inside `$COMPOSE_ROOT`? If yes,
the agent created it (via register or copy — it is the only writer there), can read and rewrite
its files, and reports `managed:true` so a UI can offer in-place editing. If no, the stack was
provisioned by something else, its files are outside the agent's mount, and it is reported
`managed:false`. The check is prefix-*and*-subdirectory correct: a sibling directory that merely
shares a path prefix is not "under" the root. It needs no migration and no persisted provenance
flag, which is exactly why it is structural.

**What the agent will do with a project, and why.** Every project in the fleet snapshot and in
`GET /v1/projects` carries its capability from one function (`projectCapabilities`,
`capability.go`), and every mutating endpoint enforces the same function, so a consumer keyed on
what the agent advertises never offers an op the endpoint refuses:

| Field | Meaning |
|---|---|
| `allowed_ops` | Sorted `POST /v1/projects/:name/op` values accepted now. Always present; `[]` means none. |
| `operable` | `allowed_ops` includes the file-loading ops (`up`/`pull`/`recreate`/`update`). |
| `managed` | The files can be read and rewritten here (edit, copy source). |
| `ops_blocked` | Why `allowed_ops` is not every op, by priority: `self`, `invalid_name`, `outside_compose_root`, `control_path`. Absent when every op is allowed. |

The rules, per op rather than per stack:

- **`restart` and `down` do not read files** — compose rebuilds them from the containers' labels —
  so they work on a stack outside `$COMPOSE_ROOT`. `up`/`pull`/`recreate`/`update` load the compose
  model, so they need the files under the mount.
- **The agent's own stack allows nothing**, wherever its files live, and its own container refuses
  every verb: compose or the daemon would stop the container running the job, the process dies
  mid-operation, and the host is left with no agent. Update the agent with whatever deployed it.
  "Own" is keyed on the live containers' compose labels, not on a registry entry, so
  re-registering a working dir cannot launder it.
- **A name compose would normalise allows nothing** (`invalid_name`): compose lowercases the name
  before `down`/`restart`, so `Docker-Agent` would act on `docker-agent`. Register and copy refuse
  such a name with `400 invalid_project_name`.
- **The control-path proxy** — the container `TRAEFIK_DOCKER_DNS` names — refuses `down` on its
  stack and `stop`/`kill`/`remove` on the container: the agent would be alive but unreachable with
  nothing able to start the proxy again. `restart`/`recreate`/`update` bring it back and are allowed.

Self identity comes from `/proc/self/mountinfo` (the container ID) plus the container **list**
(the compose project, and every container sharing the agent's network namespace, which is treated
as self because Docker gives such a container the owner's mountinfo). There is no separate inspect
and nothing waits under a lock: `GET /health/ready` reports the last published view in its `self`
block and never gates on it. A mutating request takes a fresh bounded list first; if the agent knows
its container ID but cannot list, the request is refused `503 self_identity_unavailable` rather
than allowed blind. If mountinfo names no container (not running under Docker), nothing is
protected by identity and readiness says so — that case cannot tell what to protect, and refusing
every op would take the whole agent down with it. Container verbs resolve each target through the
daemon (`inspect` → full ID), so a container named `db` is not the agent just because the agent's
ID starts with `db`.

| Refusal | Status | `code` |
|---|---|---|
| Op not in `allowed_ops` (non-self reason) | 409 | `project_not_operable` |
| Any op, register, copy source/target on the agent's own project | 409 | `self_project` |
| Copy of a project whose files are not visible | 409 | `project_not_editable` |
| Container verb on the agent's own container | 409 | `self_container` |
| `stop`/`kill`/`remove` on the control-path container | 409 | `control_path_container` |
| Register/copy with a name compose would normalise, or longer than 255 bytes | 400 | `invalid_project_name` |
| Register/copy onto an existing project without `replace:true` | 409 | `project_exists` |
| Register: relative, or longer than 4096 bytes, `working_dir` | 400 | `invalid_working_dir` |
| Register: a declared compose/env path escaping the working dir | 400 | `project_path_outside_working_dir` |
| Register: an inline file path that is empty or has a >255-byte component | 400 | `invalid_file_path` |
| Register: a path-only project under the root with no readable compose file | 400 | `compose_files_missing` |
| Unknown project (op, copy source, bundle) | 404 | `unknown_project` |
| Malformed body, or a required field missing | 400 | `invalid_body` |
| Op not one of the six | 400 | `invalid_op` |
| `health_timeout_s` / `swap_timeout_s` out of range | 400 | `invalid_budget` |
| `override_image` not an image reference by docker's own grammar (`distribution/reference`), or an image ID (`sha256:…`) | 400 | `invalid_override_image` |
| Register or copy while another change holds the project for longer than 10s | 409 | `project_busy` (`"retryable": true`, `holder`) |
| Register or copy onto a working directory another registered project already has (symlinks resolved) | 409 | `working_dir_in_use` (`project`, `working_dir`) |
| Op `services`: not a compose service name, or not a service of the project | 400 | `unknown_service` (`service`) |
| Bulk `action` not one of start/stop/restart/kill/remove | 400 | `invalid_action` |
| More than 100 distinct targets | 400 | `too_many_targets` |
| An ambiguous, malformed, or >255-byte container reference | 400 | `invalid_target` |
| Cancel of a job that is unknown or already finished | 409 | `job_not_cancellable` |
| The container list or a target inspect failed | 503 | `self_identity_unavailable` |

**Refusal semantics.** Every 4xx carries a `code`, and a coded 4xx is **final**: retrying the same
request gets the same answer. The exceptions say so with `"retryable": true` — only
`idempotency_key_in_flight`, whose answer is still coming, and `project_busy`, whose project is
being changed by the job or request named in `holder`; a new retryable code is a contract change
for every consumer (the test suite pins the set). A failure that is not the caller's — a request
body that could not be read, a write that failed, the Docker daemon not answering — is a 5xx, and
only `503 self_identity_unavailable` carries a code there. Refusals echo caller input clipped to 512
bytes. Project refusals carry `working_dir`; container refusals
carry `targets`. A bulk request naming any protected container is refused whole. The idempotency
codes are under Idempotency-Key.

---

## API consumers

| Consumer | Where | Uses |
|---|---|---|
| control-center | the router's `/api/docker/:node/*` proxy; the frontend's stack editor, copy and fleet pages | register (`replace:true` from the editor), copy, ops, container verbs; reads `allowed_ops` / `operable` / `managed` / `ops_blocked` and refusal `code`s |
| Ansible | `plugins/module_utils/docker_agent_client.py` — the one client behind the `docker_agent_stack` module and `system_monitor` | register (`replace:true`), ops |
| Test harness | control-center `tools/docker-agent-test` | every endpoint; asserts `409 project_exists` without `replace` |

### Rollout order: consumers first, agents last

A contract change ships to every consumer **before** any agent runs it. The changes are safe in
that direction and only in that direction:

- **A request field the agent now requires.** Registering onto an existing project needs
  `"replace": true`. An old agent binds request bodies with `ShouldBindJSON` and no
  `DisallowUnknownFields`, so it ignores `replace` — a new consumer works against it. An old
  consumer against a new agent gets `409 project_exists` on every re-register: Ansible's re-run
  of a deployed stack fails, and so does an editor save.
- **Response fields the agent now adds** — `allowed_ops`, `operable`, `ops_blocked`, a refusal's
  `code` and `retryable`. A consumer deployed first reads an old agent's responses without them,
  so it must treat a missing field as *not reported*, never as a refusal or as "nothing allowed".
- **`Idempotency-Key`** is optional. An old agent ignores the header, so a consumer that sends it
  gets replay protection only once the agent is upgraded, and must not depend on it before then.
- **`409 project_busy`** is new and retryable: a register or copy onto a project a job is
  changing. A consumer should retry after the named holder finishes; one that does not yet know
  the code surfaces it as an error, which is safe — nothing was written. It is never recorded
  under an Idempotency-Key: the same key goes through once the project is free.
- **`409 working_dir_in_use`** is new and final: two registry names for one directory are
  refused. The project lock is keyed by the resolved directory, so a duplicate that predates the
  rule still cannot interleave with its twin.
- **`services` on an op** is new; an agent advertises it with `"service_ops": true` on each
  `GET /v1/projects` entry. An older agent ignores the field and acts on the WHOLE project, so a
  consumer must check `service_ops` before sending it — not after.
- **`/bundle` returns file contents verbatim** — every compose file and every project-level env
  file the load reads. Neither may carry a secret inline: a secret reaches a stack through a
  service's `env_file:` rendered from Bitwarden, which compose reads at `up` and `/bundle` never
  returns. A secret in a compose file or the project `.env` is sent to whoever reads the bundle.
- **`override_image` validation is stricter**: docker's reference grammar, and no image IDs. A
  value control-api accepted and the old agent took (`traefik:`, `sha256:…`) is now `400
  invalid_override_image`.

So the order is: control-center (router and frontend, both sites) → the Ansible release
(`docker_agent_client.py`, rolled out to the fleet) → the agents, host by host, through Ansible —
the agent is never updated through its own API. The harness asserts the new refusal, so run it
against an upgraded agent.

---

## Controller contract

The agent is not standalone: it expects a controller implementing five endpoints under the
`CONTROL_CENTER_URL` base. Every call carries `Authorization: Bearer <token from
BEARER_TOKEN_FILE>`, attached by a round-tripper on the one shared pooled client.

| Call | When | Body / purpose |
|---|---|---|
| `POST /api/docker/jobs/:id/event` | Every job lifecycle transition | The job's full state (`started`/`progress`/`completed`/`failed`/`cancelled`). Delivered through a **durable sqlite outbox** — a controller outage delays events, it does not lose them. |
| `POST /api/docker/reconcile` | Boot, then every 5 minutes | This agent's authoritative recent job set, so a terminal event that was lost in flight can't strand a row as "running" forever. |
| `POST /api/docker/discovery` | Every `DOCKER_DISCOVERY_INTERVAL`, plus after each image-check pass | The full snapshot: containers, compose projects, docker networks, and the proxy container's network attachments. Latest-wins — the controller replaces every record for this `(site, node)`. Deliberately **not** outboxed: a missed push self-heals on the next tick. |
| `GET /api/docker/strategies` | Before each image-check pass and each update resolve | The central image-strategy overrides. Best-effort: on failure the previous set is kept. |
| `POST /api/docker/github-token` | When the cached token is empty or within 5 minutes of expiry | Vends a GitHub App installation token (`{"token":…, "expires_at":…}`) so the agent can query GitHub authenticated without holding an App private key. Retried 4× with exponential backoff on transient failures; a 401/403 fails immediately. |

**If you do not have such a controller**, the agent still runs and its whole local API works: job
events queue up in the outbox, discovery pushes log a warning on transition, and image checks
fall back to registry-digest detection for anything that does not need GitHub. But you would be
running it without the half that makes it a *fleet*, and you would need to stand up those five
endpoints (or fork out the outbound calls) to get that back.

### The dial rewriter

If `TRAEFIK_DOCKER_DNS` is set, the client's `DialContext` ignores the URL's host:port and dials
`<TRAEFIK_DOCKER_DNS>:<TRAEFIK_DIAL_PORT>` instead, while leaving the `Host` header and TLS SNI
as the original URL host. That lets the host's own local reverse proxy match its routing rule
and attach the mTLS client identity on the way out — so **the agent never holds a client
certificate**. Leave it unset to dial the controller directly.

---

## Build & run

```bash
go build ./...        # works with no special setup
go test ./...         # hermetic
```

Running it needs, at minimum, `SITE_ID`, `CONTROL_CENTER_URL`, a bearer-token file, a hostname of
the form `<SITE_ID>-<node>`, and the docker socket mounted.

### The container build currently needs a GitHub token

`github.com/Daniel-dev22/agent-kit-go` is a **private** module, so `go mod download` inside the
build cannot fetch it anonymously. The Dockerfile therefore takes the token as a buildx secret:

```bash
docker build --secret id=ghtoken,src=/path/to/token-file -t docker-agent .
```

A credential helper reads it from the tmpfs mount at fetch time, so the token never lands in
`.gitconfig` or in any image layer. The mount is declared `required=true` deliberately: without
it the build fails loudly instead of silently attempting an anonymous (404) fetch and producing
a confusing error later.

Once `agent-kit-go` is public, the `ENV GOPRIVATE=...` line and the `--mount=type=secret` block
can both be deleted and the build becomes an ordinary `docker build`.

The image is a two-stage build on `golang:1.26-alpine` → `alpine:3.21`, ships a static
`CGO_ENABLED=0` binary (the sqlite driver is pure Go), exposes `8080`, and has a `HEALTHCHECK`
on `/health/ready`.

---

## Testing

`go test ./...` is **hermetic** — no Docker daemon, no network, no credentials. Ten test files:

| File | Covers |
|---|---|
| `github_test.go` | Raw-GitHub URL → Contents API rewrite, `Retry-After` parsing, transient-failure retry and backoff, context cancellation, the Contents API path, `releaseQueryFor`. Uses `httptest` servers. |
| `imagecheck_test.go` | Image-reference parsing (incl. digests), pinned-tag classification, semver ordering, `versionOutdated`, OCI-label source-repo derivation, auto-detect strategy selection, commit-SHA tag recognition, owner/package splitting, override precedence, tag variants, tag filter/dedupe, local digest resolution, and the retain-last-good-plan degradation. |
| `imagecheck_force_test.go` | The refresh endpoint's force debounce window. |
| `resolvers_test.go` | Upstream-compose caching by URL, refetch on a new tag, cache/vars independence, retry through a 429, not caching failures, `planServiceTarget`'s three outcomes, compose var interpolation. |
| `resolvers_live_test.go` | *(env-gated, see below)* |
| `stackengine_test.go` | Resolver-kind derivation, baseline helpers, `applyImages`, the per-host-class health timeouts, `notHealthy`, override-resolver service inference, env-var-reference extraction, and `setServiceImage` for both the literal-scalar and `${VAR}` cases against real temp files. |
| `project_plan_test.go` | The plan-driven detection rule: a coupled stack whose driver is current reports *updated* even when a coupled dependency has its own newer upstream — and the converse when the driver does move. |
| `movingtag_test.go` | Moving-tag recognition. |
| `projects_managed_test.go` | `underComposeRoot` (including the prefix-but-not-subdirectory case), `mergeKnown`'s `managed` flag for owned vs external stacks, and `readBundle`'s graceful degradation on an unreadable compose file. |
| `capability_test.go` | The mountinfo parser, self-view derivation (network-namespace dependents, control path by name/alias), the per-op capability table, the snapshot wire contract (`allowed_ops:[]`, `false` on the wire), and every refusal through the real routes and a real `dockerClient` against a fake Engine API — including the production `newApp` wiring, readiness under a hung daemon, list/inspect failure, and file-write-before-refusal. |
| `logstream_test.go` | The line writer (including a line longer than any single read), `drainBatch` coalescing, and an end-to-end backlog-then-live delivery over a real WebSocket. |

### Suites that are not hermetic

Three smoke suites are behind the `smoke` build tag and need a **live Docker daemon**. They
create throwaway projects and leave real stacks alone:

```bash
DOCKER_HOST=unix:///var/run/docker.sock go test -tags smoke -run TestComposeSmoke     -v .
DOCKER_HOST=unix:///var/run/docker.sock go test -tags smoke -run TestUpdateSmoke      -v .
DOCKER_HOST=unix:///var/run/docker.sock go test -tags smoke -run TestImageCheckSmoke  -v .
```

- `compose_smoke_test.go` — the in-process compose binding end to end.
- `update_smoke_test.go` — the full update engine, using the override resolver so it needs no
  GitHub or registry version resolution: deploy `alpine:3.19`, `update` to `alpine:3.20`, then
  assert the container swapped, came up healthy, and the compose file on disk was rewritten.
- `imagecheck_smoke_test.go` — the registry-digest source and the existence gate against real
  Docker Hub (read-only).

One more is gated on an environment variable rather than a build tag: `resolvers_live_test.go`
runs the real upstream-compose path against GitHub itself and is skipped unless
`DOCKER_AGENT_GH_TOKEN` holds a GitHub App installation token.

CI (`.github/workflows/ci.yml`) runs `go vet ./...` and `go test ./...` only — none of the above.

### What is not tested

Being honest about the gaps: there are **no tests for the HTTP handlers**, the event
buffer/outbox and its sqlite schema, the discovery pusher, the network gather, the heartbeat, or
the orphan sweep. The unit tests concentrate on the version-resolution and update-planning
logic, which is where the subtle bugs were; the transport and persistence layers are covered
only by the smoke suites and by running it.

---

## Related projects

- [agent-kit-go](https://github.com/Daniel-dev22/agent-kit-go) — the shared Go module this and
  its siblings build on (job store, event outbox, fleet hub, reconcile, scheduler, logging).
- [build-agent](https://github.com/Daniel-dev22/build-agent) — per-host container image builds
  as agent jobs.
- [duplicacy-agent-api](https://github.com/Daniel-dev22/duplicacy-agent-api) — per-host backup
  scheduling and execution.
- [gdrive-agent](https://github.com/Daniel-dev22/gdrive-agent) — per-host Google Drive sync jobs.
- [filemesh-agent](https://github.com/Daniel-dev22/filemesh-agent) — per-host file
  synchronisation.

---

## License

MIT — see [LICENSE](LICENSE).
