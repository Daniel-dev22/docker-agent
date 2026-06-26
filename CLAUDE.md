# docker-agent — AI instructions

Per-host container-fleet agent (Go/gin) deployed as docker-compose on every
docker host (NUC/Pi/VM/NAS). Mounts `/var/run/docker.sock`, listens on `:8080`
on `traefik-network`. Cross-cutting agent rules (outbound dial rewriter,
bearer-token auth, agent-kit-go) live in the controller repo's `CLAUDE.md`
"Standalone Agent APIs" section — read that first for anything deploy/auth.

## API consumer contract — system_monitor (READ BEFORE CHANGING ANY ENDPOINT)

`system_monitor` (the Ansible repo's `system_monitor/` service) is now a
first-class consumer of this agent's API — it replaced the retired Portainer
integration. It talks to the agent **directly** the way it talks to the Traefik
API: it resolves the `docker-agent` container IP via `docker inspect` and
`curl http://<ip>:8080{path}` on the host (local or over SSH for the NAS),
bypassing Traefik's mTLS gate. So it sends **no bearer token and no client
cert** — do not add an inbound auth requirement on these routes without
coordinating, or you will silently break it.

**HARD RULE: before you change the shape or behaviour of any endpoint below,
check what `system_monitor` relies on and update it in lockstep.** A breaking
change to this API is not "done" until `system_monitor` is updated and verified
against it. Treat the response fields listed here as a contract.

Endpoints + fields system_monitor depends on:

| Endpoint | Method | system_monitor relies on |
|---|---|---|
| `/v1/projects` | GET | `projects[].name`, `projects[].working_dir`, `projects[].compose_files` |
| `/v1/projects/:name/bundle` | GET | `compose_files[].name`, `compose_files[].content`, `env_files[].{name,content}` |
| `/v1/projects/:name/op` | POST | body `{op:"up", trigger_key}` → 202 with `job_id` (+ `state`) |
| `/v1/projects` | POST | body `{name, files:{relpath:content}, deploy:true}` → `registered` (+ `job_id` when deployed) |

What it uses them for:
- **docker-network monitor** — lists projects + reads each compose to find stacks
  using monitored (ipvlan/bridge) networks; on a network mismatch it force-removes
  the affected containers then `POST /v1/projects/:name/op {op:"up"}` to redeploy.
- **TrueNAS frigate remediation** — reads the frigate project bundle, rewrites the
  media-drive volume paths, then `POST /v1/projects {name, files, deploy:true}` to
  rewrite the compose and redeploy.

Notes / gotchas:
- `GET /v1/projects` returns the **durable registry** only, not ad-hoc running
  projects. If you ever narrow/rename what it returns, system_monitor's stack
  discovery silently shrinks — flag it.
- `system_monitor` tolerates an empty bundle (external stack outside ComposeRoot)
  by falling back to reading the compose off disk via `working_dir` +
  `compose_files`. Keep those two fields populated for every project.

Consumer code (Ansible repo):
- `system_monitor/modules/docker_agent_utils.py` — the transport (inspect + curl).
- `system_monitor/modules/docker_network_health.py` / `docker_network_monitor.py`.
- `system_monitor/modules/truenas_monitor.py` (frigate remediation).
