# Phase 2 — docker-agent surfaces build provenance into discovery

Plan: `control-center/docs/BUILD_PROVENANCE_PLAN.md`.
Previous: `build-agent/docs/BUILD_PROVENANCE_PHASE1_HANDOFF.md` — read it, it corrects the plan.

## Status

| | |
|---|---|
| Built | yes — `provenance.go` (new), `dockerclient.go` + `imagecheck.go` (wiring), `provenance_test.go` (new) |
| Reviewed | **self-review + a 19-mutation campaign.** The four independent lenses were NOT run (this session cannot spawn subagents) |
| Merged | check `git log main` |
| Tagged / deployed | **no.** Neither this nor phase 1 has run anywhere |

**Nothing has been observed end to end.** No image carries the labels yet, because
build-agent 0.1.6 is not tagged or deployed. Phase 2 is therefore written entirely
against a contract, not against observed data — the first rollout is the first time
any of it meets a real label.

## What ships

`compose_projects` and `containers` discovery rows gain, all `omitempty`:

`source_repo` · `source_ref` · `source_revision` · `build_context` · `built_from_source`

## Two places this departs from the plan, both deliberate

**1. It reads the IMAGE's labels, not the container's.** The plan said "docker-agent
reads `Config.Labels` off the running container". It does not: `checkUnit` already
receives an `imageInfo` carrying `resp.Config.Labels` from the imageChecker's slow
capped pass, which inspects every unique image exactly once. Reading there costs
**zero additional Engine API calls**, keeps the fleet snapshot's single bounded
`ContainerList` untouched (the Pi-safe posture `dockerclient.go:38` commits to), and
cannot be shadowed by a `labels:` block in a compose file.

**2. 🔴 `provenanceFromLabels` returns NOTHING unless `com.controlcenter.build.context`
is present.** This is the most important line in the phase.
`org.opencontainers.image.source` and `.revision` are **standard** labels that a
large fraction of third-party images already set — everything built by
`docker/metadata-action` does, and ghcr uses `.source` for repository linking. Read
naively, an upstream Frigate image's own labels would flow into discovery, pre-fill
the rebuild form in phase 3, and hand a ref we never built to `git clone --branch`.
Only build-agent sets the namespaced label.

These labels are **not authenticated** — any image can claim any of them. The gate
makes the claim *namespaced*, not *trusted*. Consumers must still validate
(control-api's `validBranch` does).

## The rollup rule: unanimous, field by field, or absent

A project's field is published only when **every** container in the project reported
the same non-empty value for it.

| stack | published |
|---|---|
| single service, built by us | everything |
| all services, same repo+ref+commit | everything |
| rebuilt at two commits | repo + ref; **revision dropped** |
| one service on a different ref | repo; ref dropped |
| one pulled sidecar | **nothing** — it cannot be rebuilt from one ref |
| first check pass still in flight | **nothing** until every container has a result |

Fields fold independently because dropping a unanimous repo and ref over a
disagreeing revision would discard exactly what the form needs.

## What was verified, and how

**Measured — 19 mutations, 19 caught**, including the gate removed (upstream labels
leak), the gate widened, a label-key typo (contract drift), divergence never
detected, the denominator ignored and off by one, `built_from_source` always true
and mis-cased, `omitempty` dropped, and the independent folds collapsed into one.
`go test -race` clean, 79 tests.

**Two rounds of that campaign found defects in the CODE, not gaps in the tests** —
which is the part worth carrying forward:

- **Two guards enforcing one invariant, each masking the other.** Unchecked
  containers voted "empty" *and* a container-count denominator was compared. Mutating
  either left the suite green, because the other still produced the right answer.
  Collapsed to one load-bearing guard (the denominator); the mutations then bit.
- **Two clauses that could never change an outcome** (`total <= 0`, and a
  `val == ""` test whose only reachable case already returned `val`). Removed rather
  than tested — a guard that cannot fire is noise, and it was noise sitting directly
  under a comment complaining about the same thing.

**The tests drive the composition, not only the ends.** `stampImageStatus` is
exercised assembling a mixed stack and a half-finished check pass, and there is a
test for **`mergeKnown`** — which runs *after* the stamping and is the last thing to
touch a project row before it reaches both the fleet frame and the discovery push.
It mutates matched rows in place today; a rebuild of the struct there would drop
this entire phase with every other test in the file still green.

**Assumed, NOT measured:**
- That any image anywhere carries these labels. None does yet.
- That the router and discovery-api pass the new fields through untouched. Phase 0
  read the code and found verbatim pass-through (`attributes = []byte(raw)` →
  `map[string]interface{}`), but no row has actually carried them.
- Everything phase 1 left unverified (buildx accepting the argv, cache behaviour,
  multi-arch manifests) still gates this: if a label does not land, this reads empty
  and looks exactly like "nothing rebuilt yet".

## Decisions

- **Label keys duplicated, not hoisted into `agent-kit-go`.** Both agents depend on
  the kit, and the estate rule says shared code goes there — but the kit is a public
  module and `com.controlcenter.*` is this estate's namespace, so the wire-format
  carve-out applies. Both repos pin the literals in a test naming the counterpart,
  which makes a change on either side deliberate. Recorded as **D7** in the plan;
  revisit if a third consumer appears.
- **Provenance recorded before `checkUnit` branches on strategy**, so a check that
  fails its registry/GitHub lookup still carries the source.

## Next phase's first concrete step

Phase 3, in `control-center/schema-api/main.go`, the `docker-stack-update` schema
(~line 1217): add `"populateFields": true` to `stack_name`'s `x-discovery`, then
`x-discovery-field` on `override_image_name` (`latest_image_version`),
`frigate_custom_branch` (`built_from_source`) and `frigate_branch_name`
(`source_ref`), and delete the inert `x-auto-populate` block. Phase 0's answer 1 has
the mechanism and the three frontend traps.

**Do not ship phase 3 before phase 1+2 are deployed and a discovery row is observed
carrying `source_ref`.** Phase 3 is the first thing a user sees; if the fields are
empty it silently degrades to today's manual form and nobody learns that the
pipeline is broken.

## Traps

- **A stack whose image predates phase 1 is indistinguishable from a broken
  pipeline.** Both look like "no provenance". The way to tell them apart is to check
  the image itself: `docker inspect --format '{{json .Config.Labels}}'`.
- **`built_from_source` must stay a quoted string on the wire.** There is a test
  asserting `"built_from_source":"true"` and rejecting `:true`.
- **The rollup is silent by design when a stack is mixed.** That is correct and it
  has no counter — if someone reports "frigate stopped pre-filling", check for a new
  sidecar container in the project before suspecting the labels.
- **docker-agent is at 0.1.14.** Same two-playbook deploy shape as build-agent; it
  has no k8s deployment, so `build-deploy.yml` alone leaves the old container running.
