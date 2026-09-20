# Agent stack ops — Phase 3 handoff: ownership survives losing the index

Plan: `docs/AGENT_STACK_OPS_PLAN.md` (this repo), updated to match what was built.
Phase 1: `docs/AGENT_STACK_OPS_PHASE1_HANDOFF.md`. Phase 2: `docs/AGENT_STACK_OPS_PHASE2_HANDOFF.md`.

One sentence: **deleting `projects.json` on a live host can no longer make an Ansible-rendered
stack editable**, because the owner is recorded in the directory it governs.

## Status

| | docker-agent | ansible | control-center |
|---|---|---|---|
| Branch | `feat/owner-durable` (merged, deleted) | `feat/nut-owner-gate` + `feat/nut-host-correction` (merged, deleted) | — no change needed |
| Code commits | 2 (`a0ef577` build, `b725d69` review fixes) | 3 (`e798722d`, `3a25bfc8`, `9bb3ff03` post-deploy corrections) | — |
| Merged | see §Merge record | see §Merge record | — |
| Released / deployed | **`0.1.20`, LIVE on all 10 hosts** — see §Rollout record | **`51.107.10` + `51.107.11`, distributed to kd-cluster + ng-cluster** — see §Rollout record | — |
| Review | 4 lenses commissioned, 3 completed, 1 stopped (see §Surprises); every finding fixed or recorded below |

**The wire is unchanged.** `owner` was already on `/v1/projects` and the fleet snapshot from Phase 2;
this phase only changes where the agent gets it from. No control-center change, no frontend change,
no ansible module parameter change.

## Why this phase exists — the reproduction, measured

Phase 2 put `ProjectEntry.Owner` in `projects.json` and nowhere else. On a throwaway agent
(2026-09-20, its own ComposeRoot, own port, dead controller URL — a production agent was never
touched):

| step | before this phase |
|---|---|
| register `phase3-owned` with `owner: "ansible"` | `owner:"ansible"`, `managed:false`, all six ops |
| `rm projects.json`, restart, containers still up | **`owner:null`, `managed:true`** — and the unowned entry is PERSISTED |
| the editor's own call (register + `files`, no owner) | **200, and the compose file on disk was rewritten** |
| the same call with the owner recorded | `409 project_owned`, file untouched |

And the same laundering with **no file loss at all**: `DELETE /v1/projects/phase3-owned` succeeded
with no owner check, and the very next fleet snapshot read `managed:true` with the container still
running — no restart, no credential, on an API that is unauthenticated on the host's docker network.

The risk was live, not theoretical: `duplicacy-agent-api` on kd-nuc01 already carries container
labels pointing **under** the ComposeRoot, so it was one lost file away from an editable
Ansible-rendered stack. (Phase 2's handoff said the converge does not relabel and the window
therefore fails safe. True when written; that host has since been through an update.)

## What shipped

### docker-agent — the owner is a property of the directory

- **`ownermark.go`** — `<working_dir>/.docker-agent-owner`, written by `composeRegistry.register`
  for *every* caller, so no path can record an owner the directory does not. Cleared by the
  register that clears the owner. Read confined under the ComposeRoot, `O_NOFOLLOW` (never a link),
  `O_NONBLOCK` (a FIFO cannot hang the snapshot path), and it must be a regular file.
- **Every live-derived entry takes its owner from the directory** — boot adoption, an op resolved
  against a name the index lost, and an unregistered row in the fleet snapshot.
- **Registry load reconciles**, and the directory wins. It also **migrates**: an entry whose owner
  the index knows and the directory does not gets its mark written there, so every stack the fleet
  has already converged is protected on the **first restart after this ships** — no role run, no
  operator step. Verified live.
- **One predicate, `app.ownerOfFiles`**, answers "who do these files belong to right now" for every
  guard that writes, drops or overwrites them: the register's two checks and the copy's destination.
- **The mark is reserved as a bundle path component**, so nothing that can push files can forge one.
- **Readiness reports `projects.owners_not_recorded`** — ownership the index holds that the
  directory cannot back up, which is otherwise silent until the day it matters.

### ansible — the nut stack declares its owner on its own condition

Phase 2's deferred row said nut "will take its owner at its next template change". **Re-measured,
and the implication was wrong**: the register that declares the owner is gated on the compose
*content* differing, so a host whose template is already converged never declares an owner at all.
Two days after the owner shipped, **kd-nuc02**'s `nut` stack still read `owner=null, managed=true`,
and Control Center was still offering Edit on files that play reverts every 25 minutes.

⚠ **I first named the wrong host, and the correction matters.** `nut_ups_hosts` is `[kd-nuc02]` —
that is the only host this file renders. **ng-nuc01 is in `nut_legacy_device_sync_hosts`**, whose
compose this repo deliberately does NOT render, so `owner=null` there is CORRECT and must stay that
way. I measured ng-nuc01, saw a true symptom, and attributed it to the wrong role; a project filter
that only listed stacks which already had an owner is what hid kd-nuc02's from me. Fixed in
`51.107.11`, along with a revert described below.

The two decisions are now separate — the owner opens the gate on its own; only a content change
earns a `deploy`, because recording a name must not restart the UPS monitor. `ups/tests/` is new
(**17** tests that read the decisions out of the YAML and *evaluate* them — 15 for the owner gate,
plus 2 added with the correction below that refuse an owner on the legacy path), and `ups/**` was
added to both `paths` filters and the pytest invocation in `plugin-tests.yml`, because it was in
neither.

`docker_agent_client.py`'s register docstring no longer claims `/bundle` "serves nothing" for an
owned stack — it serves the files, and that distinction is what broke the nut converge once.

⚠ **`legacy_device_sync.yaml` must NOT declare an owner**, and briefly did. A review lens suggested
adding `owner: 'ansible'` for symmetry with the owner path; I applied it without reading what
"legacy" means. That file handles a compose this repo does not render — it rewrites
`services.nut-ups.devices` and leaves every other key as the host has it, "so converging a site onto
the template stays a deliberate act rather than a side effect of the 25-minute schedule". Declaring
an owner asserts the opposite and takes the stack out of the editor on the one kind of host where a
human is meant to edit it. It shipped in `51.107.10`, was inert (the task is gated on the USB device
id changing, which no host did), and is reverted in `51.107.11` with two tests that refuse it next
time. **Symmetry between an owner and a non-owner is the bug.**

## What was verified, and how

**Measured on a running agent** (throwaway instance, image built from `b725d69`), all re-run after
the review fixes:

- the reproduction above now answers `owner:"ansible"`, `managed:false`, all six ops;
- the editor's exact call → `409 project_owned`, **file byte-identical** (checked by md5);
- a bundle path `".docker-agent-owner/x"` → `400 invalid_file_path`;
- `owner:"unknown"` → `400 invalid_owner`;
- deregister → the fleet snapshot **still** reads `owner:"ansible"`, `managed:false`, with the
  container running and no restart;
- **the migration**: an index with an owner and no mark on disk → `recorded the owner in its stack
  directory` at boot, mark present;
- **an unreadable mark** (a directory at that path) → `owner mark … is not a regular file`, the
  index keeps `ansible`, nothing is persisted, and readiness lists the stack.

**Suites:** Go `./...` green (631 tests, 31 of them new in `ownermark_test.go`), `-race` green on
the new tests.

Ansible, run at the RELEASE POINT rather than at a branch tip — a tag ships the repo, not the diff:
**1,724 passed at `51.107.11`**, the tag this phase shipped. The number first written here was
1,722, measured at the `feat/nut-owner-gate` merge and then left standing while the correction
commit added two tests: a measurement expires with the code it was taken against, and a fix is the
fastest way to invalidate one. Re-run later at `main`/`51.107.14`, which carries other sessions'
work on top: **1,730 passed** — so nothing of this phase's went red under theirs.

**Canaries.** 16 mutations of the build, then **15 of the review FIXES** — the round that matters,
because a review fix is a change like any other. Two survived first and both were real gaps:
`setOwner`'s in-place write (invisible because its only caller runs before the listener binds — now
pinned by the *invariant*, an indexed entry is immutable, under `-race`), and reading marks for
indexed rows too (every test tolerated it; it silently drops a recorded owner whose mark is
missing). A third fix, the regular-file check, is **not independently falsifiable** — the read fails
on a directory anyway — so the test asserts what it actually earns: the failure *names the cause*.

**Measured on the fleet after release** — see §Rollout record. The migration, the mark on disk, and
the nut owner are all observed in production; what remains unexercised there is the reproduction
itself (nobody has deleted a live `projects.json`, and nobody should).

## Decisions, and the alternatives rejected

- **The owner is recorded in the directory it governs.** Rejected: **a compose label** the agent
  reads back (the plan's option (a)) — it is a genuine second *declaration* that can disagree with
  the register, every future owned stack must remember to add it, and it only appears once
  containers are recreated, which Phase 2 measured the converge deliberately does not do. So it
  would be absent exactly during the window it exists to cover. Rejected: **making the loss loud**
  (option (b)) — an alarm on an open door; it leaves the defect live and relies on someone reading
  readiness. Rejected: **"an adopted entry is never editable"** — fail-safe and needs no new file,
  but it is the *wrong* answer for the ~11 agent-authored stacks per host, which would lose Edit
  after a loss with no automatic recovery.
- **It is not a second source of truth.** There is still exactly one declaration — the `owner` on
  the register call. The mark is that record kept beside the files instead of in an index that can
  be lost separately from them. The index and the directory back each other up: lose either and the
  next load restores it; lose both and the next converge re-declares it.
- **No deregister guard.** Written first, then removed — see §Surprises. The plan said it: with
  structural recovery a deregister cannot launder ownership.
- **`unknownOwner` is RESERVED.** `validOwner` refuses it as an input, so "unknown" on the wire has
  exactly one meaning: the agent could not read the mark. A sentinel that doubles as a legal
  declaration is one every future reader of `Owner` has to remember.
- **A failed read is a reason to refuse a write, never a fact to store.** It fails safe in memory
  and reaches nothing durable.
- **Ansible's owner declaration is gated on the owner, not on the compose content.** Rejected:
  registering unconditionally — the play is fanned out every 25 minutes and a redeploy each time is
  what the content gate exists to prevent.

## Surprises

- 🔴 **A review lens wiped my fixes mid-session.** The verification lens was told it could mutate
  files to run a mutation sweep provided it restored them — in **the same worktree I was editing**.
  It restored `compose_handlers.go` and `ownermark.go` from its own pre-fix copy, silently reverting
  three completed fixes; the tests I ran next failed against code I had already written, which
  looked like my edits had never applied. Caught by a presence-grep for each fix, re-applied, and
  the lens stopped. **A reviewer that mutates files needs its own worktree** — `phased-delivery`
  says so and I did not do it.
- 🔴 **My own guard was the merge blocker.** The reserved-name check tested the *basename*, and
  `writeProjectFiles` MkdirAll's a path's parents before confining it — so a bundle path
  `".docker-agent-owner/x"` created a **directory** at the reserved name. It then read as owner
  "unknown" (EISDIR), and neither the clear (ENOTEMPTY) nor the atomic rename (EEXIST) could remove
  it: un-editable, un-registerable, unrecoverable through the API, from three unauthenticated
  calls. Reproduced before fixing. The guard and the code that consumes the path disagreed about
  what a path *is*.
- 🔴 **Three lenses independently found the same defect from three directions**: the read-time
  sentinel was being persisted, and the next boot's migration branch then wrote it into the
  directory as a real declaration — destroying the true owner and refusing that owner's own
  converge, with readiness reading healthy because a mark was present. The worst trigger is
  ordinary: `confinePath` resolves the compose root, so a root briefly unresolvable (a mount not up
  yet at boot) fails that way for **every entry at once**, including unowned ones.
- **The deregister guard I added created three cycles with no exit through the API.** A shared
  working directory became permanently un-deregisterable (both entries refuse, and the documented
  way out — a path-only register with `"owner": ""` — is itself refused by `working_dir_in_use`); a
  stack whose files had been removed could not be deregistered at all; and it put a retryable
  `project_busy` on a verb whose one external client only retries POSTs. It protected nothing the
  directory did not already protect. **Refusing reads as safer and was not.**
- **The plan named the wrong stack, and then so did I.** The plan says `nut-ups`; the project is
  `nut`, so a grep for the plan's name finds nothing. I then compounded it: I measured ng-nuc01,
  found `owner=null`, and wrote it up as the unowned repo-owned stack. It is not — ng-nuc01 is the
  LEGACY host and is correctly unowned. The repo-owned stack is **kd-nuc02**, and my own project
  filter (which listed only stacks that already had an owner) is what hid it. A true reading of the
  wrong subject looks exactly like a measurement. Corrected in `51.107.11`.
- **`traefik` is `managed:true` on all six kd hosts.** Expected — it is the gated Phase 4 stack —
  but worth seeing written down: the UI offers Edit on it today, on every host.

## Deferred — with the measurement, not just the verdict

| Item | Measurement / reason | State |
|---|---|---|
| 🔴 **Literal secrets in the traefik compose**, served by `/bundle` on every host | unchanged from Phase 2: readable on 10/10 hosts. **Phase 4 is gated on this.** | OPEN — the sharpest item in the register |
| `owner:"unknown"` renders in the UI as *"unknown renders this stack's compose files"* | the frontend prints `project.owner` verbatim. It can now only appear **while a mark is unreadable** (never persisted), and the Edit button is correctly disabled either way. A 3-line frontend change + a frontend build is a whole release lane for a transient string. | OPEN — fold into the next control-center change |
| `DockerAgentClient._await_retryable` is **POST-only** | `headers` is set only for POST, so a `retryable` refusal on any other verb is a hard error string. This phase no longer creates the exposure (the deregister lock is gone), so it is pre-existing and latent. | OPEN — pre-existing |
| `rewrite_stack_container_dns.yaml` pushes files with no owner | `dns_pinning_stacks` is `[traefik, homeassistant]`, neither owned. Adding `owner: 'ansible'` there would *declare Ansible the owner of traefik* — a Phase 4 decision, not a Phase 3 one. | OPEN, inert — Phase 4 |
| `filemesh-agent` is not built for nghome; its role targets ng-nas01 | unchanged from Phase 2 | OPEN — user decision |
| Job-history retention across the agents | unchanged; fleet-wide across 5 repos | OPEN — its own phase |
| GitHub Actions billing-blocked | every CI job reproduced locally | OPEN — user action |
| `tools/docker-agent-test` sends no `owner` anywhere | so the integration harness exercises none of Phase 2's or Phase 3's behaviour. Coverage is the Go suite only. | OPEN — worth one harness phase |

## Next phase — first concrete step

Phase 4 is **gated** and the gate is not a preference: move the three literal secrets in the
traefik compose to env from Bitwarden and **rotate them**, in the traefik role. Ownership would not
close that exposure — reads are governed by containment — so declaring traefik owned first would
look like a fix while changing nothing about the secrets.

Then, in the same change as declaring traefik `owner: 'ansible'`, fix
`server_setup/general/include_tasks/rewrite_stack_container_dns.yaml`: it reads a stack's bundle and
pushes it back with **no owner**, which is refused the moment traefik is owned. It fails CLOSED — a
silent no-op — which is the same shape that broke `nut-ups`.

This phase is already released and deployed (§Rollout record), and `owners_not_recorded` was empty
on all 10 hosts afterwards — so Phase 4 starts from a fleet where every recorded owner is durable.
Re-read that field before starting anyway; it is a state, not a one-off reading.

## Traps

- 🔴 **Deploy order: docker-agent FIRST, ansible LAST** — the same direction as Phase 2, and for a
  related reason. The nut fix makes **kd-nuc02** declare its owner; an agent that predates this
  phase records that owner only in its index, which is the state this phase exists to end.
- 🔴 **`nut_ups_hosts` is `[kd-nuc02]`; ng-nuc01 is the LEGACY host.** `owner=null` on ng-nuc01 is
  correct and must stay that way. Do not "fix" it, and do not add an owner to
  `legacy_device_sync.yaml` — there are two tests refusing that, and the reason is in them.
- 🔴 **Merging the ansible change reaches ZERO hosts.** `/home/daniel/ansible` sits at the last
  released tag, so the nut fix needs a tag **and** `git_updates/git_update_ansible_directory.yaml`
  before any host runs it. A release ships the whole repo — check `git log <last-tag>..main` first.
  This happened here: another session was holding a fleet distribute, so the window was negotiated
  rather than taken, and the release tool's own payload print confirmed each tag shipped only its
  own commits. Coordinate rather than cutting a tag into someone else's release window.
- **The first restart after this ships WRITES a file into every owned stack directory.** That is the
  migration and it is the point — but it means the agent creates `.docker-agent-owner` (root, 0600)
  inside directories Ansible renders. No role prunes unknown files from those directories today. If
  one ever does, it silently removes the mark; the index restores it at the next boot, so the
  failure is self-healing but invisible.
- **`unknown` is a reserved owner.** Do not "helpfully" allow it as an input.
- **The reserved name is reserved as a path COMPONENT.** Any new validation of bundle paths must
  keep that, because the writer creates parents before confining.
- **A reviewer that mutates files gets its own worktree.** See §Surprises.
- **Everything Phase 2's traps say still applies** — `/bundle` answers a refusal with an empty
  bundle and HTTP 200; never re-render an owned stack's `.env`; a path-only register needs the
  compose file on disk first; do not add an `owner` branch to `StackEditorDrawer`.

## Rollout record (2026-09-20)

Order was **docker-agent FIRST, ansible LAST**, per the trap above.

| # | Step | Verified |
|---|---|---|
| 1 | docker-agent `0.1.20` tagged from `main` (`1a711d1`), built on kd-nas01 + ng-nas01 as `20260920-212338` | `failed=0` both sites; the build log names commit `1a711d1` |
| 2 | `docker-agent/deploy.yml` → kdhome (6 hosts) then nghome (4) | `failed=0` on all 10 |
| 3 | **The migration ran.** kd-nuc01's log: `recorded the owner in its stack directory project=duplicacy-agent-api` | the FILE on disk: `.docker-agent-owner`, root:root `0600`, content `ansible`, mtime 17:27 |
| 4 | Fleet-wide: `owners_not_recorded` empty on all 10, 14 owned stack instances still `owner=ansible, managed=false` | checked with the field's PRESENCE as well as its emptiness — an older agent omits the key, which parses identically to healthy |
| 5 | ansible `51.107.10` released and distributed to kd-cluster + ng-cluster | `failed=0` on 7; both python daemons restarted, as the path gate predicts for a `plugins/module_utils/` change |
| 6 | `ups/manage_ups_nut_device_id.yaml -e target_servers=kd-nuc02` | the run reports **"declaring the owner"**; `nut` → `owner=ansible, managed=false`; the container's created timestamp is **unchanged** (no redeploy); a second run reports **"already converged"**, `changed=0` |
| 7 | ansible `51.107.11` (the two corrections above) released and distributed | `failed=0` on 7; `Daemons to restart: []`, correct for a `ups/`-only change |
| 8 | ng-nuc01's `nut` still reads `owner=null, managed=true` | **correct** — it is the legacy host, and that is the state it must keep |

⚠ **A peer session read the ng hosts mid-distribute and saw `51.107.10`.** Both our recaps were
consistent with the truth at different instants, so neither settled it; ssh to each host and reading
`ansible_repo_version.txt` **and** `git describe` did — all three ng hosts were at `.11`. A recap
saying `changed=9` is evidence about the run, not about the host.

## Merge record

All three branches are merged to `main`, **released, and deployed** — see §Rollout record for the
evidence. (An earlier version of this section said "neither is released, and nothing runs on a host
until it is". That was true when written and false within the hour; it is recorded here because a
"what you can trust" section that is never re-read is the first part of a handoff to rot.)

- docker-agent `feat/owner-durable` → merged as `c44f311`, released as **`0.1.20`**
- ansible `feat/nut-owner-gate` → merged as `a21865e0`, released as **`51.107.10`**
- ansible `feat/nut-host-correction` → merged as `6a2fad97`, released as **`51.107.11`**
