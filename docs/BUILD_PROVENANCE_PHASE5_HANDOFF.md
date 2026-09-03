# Phase 5 — "a build is required"

Plan: `control-center/docs/BUILD_PROVENANCE_PLAN.md`. Previous: phase 2's handoff in this repo.

## Status

| | |
|---|---|
| Built | `provenance.go`, `imagecheck.go`, `github.go`, `dockerclient.go` + tests |
| Reviewed | self-review + a 19-mutation campaign. Independent lenses NOT run |
| Merged / deployed | check `git log main`; see *What you can and cannot observe* |

## What ships

`source_status` on `containers` and `compose_projects` discovery rows:

| value | meaning |
|---|---|
| `behind` | the ref this image was built from now points at a different commit — a rebuild would change the image |
| `current` | the ref still points at the commit we built |
| *absent* | unknown — not ours, not resolvable, or the lookup failed |

## Why this is not ImageStatus

`ImageStatus` compares the deployed image against the **registry**, so it answers
*"has a newer image been pushed?"*. For an image we built ourselves from a moving
ref, **nobody pushes anything until someone rebuilds** — so `ImageStatus` reports
`updated` indefinitely while the source moves out from under it. That is the gap.

## Two departures from the plan, both deliberate

**1. Not `git ls-remote`.** docker-agent already owns an authenticated,
rate-limited, retrying GitHub client, so this is one in-process API call and no
subprocess. `resolveRef` uses `/repos/{repo}/commits/{ref}`, which resolves a
branch, a tag **or** a raw SHA — unlike the neighbouring `branchHead`, whose
`/branches/{ref}` endpoint **404s on a tag**. A test pins which endpoint is used,
because swapping them is a one-word edit that turns every tag-built image into
`unknown` with nothing failing.

**2. Not an unconditional remote lookup.** 🔴 **Every `source_ref` in production is
a version-pinned tag** (`0.1.6`, `0.1.15`), which is immutable by this estate's own
convention — already encoded in `isPinnedTag` and already relied on by the
update-strategy code. Reusing that predicate means:

- **the steady-state cost today is ZERO API calls**, and the answer is `current`
  without asking anyone;
- it starts costing, and starts being *useful*, only for a moving ref (`0.19`,
  `dev`, `main`) — the custom-build case.

## What you can and cannot observe

✅ **Observable now:** the 10 provenance-bearing rows should gain
`source_status: current`. That proves the code runs, the verdict reaches the wire,
and the pinned-tag path works — without a single GitHub call.

❌ **NOT observable until something is built from a branch:** the `behind` verdict.
No image in the estate is built from a moving ref today, so the signal this phase
exists for cannot fire. It becomes live the first time Frigate is rebuilt from
`0.19` (or any branch) via the custom-branch form.

That is the plan's own precondition — *"only worth building once phases 1–2 have
been live long enough that most stacks carry labels"* — and it is **not met**:
10 of 77 rows carry labels, and all 10 are pinned tags. Built anyway because the
mechanism is free when idle and the alternative is writing it later without the
context; but do not read a fleet of `current` as evidence that `behind` works.

## Unknown is not "fine"

A failed lookup, a non-GitHub source, a missing revision and an agent with no
GitHub client all yield `""`. Claiming `current` when the check failed would tell
an operator their image matches a source nobody managed to read. The project
rollup is **ANY-behind** (mirroring how `ImageStatus` rolls up `outdated`) and
stays absent unless at least one container actually produced a verdict.

## Verified

19 mutations, 19 caught — including the endpoint swap; an `==` in place of the
prefix compare (the two sides are different lengths **by design**, since the label
carries 40 chars and some helpers here truncate to 7, so `==` would report every
image behind); the pinned-tag skip removed; the rollup flipped ANY→ALL; and a
failed lookup reported as `current`. `resolveRef` is driven against a real HTTP
server; the stamping and rollup are driven through `stampImageStatus`.

Two survivors were found on the first pass and both were real gaps: no test drove
a **failing** remote lookup, and nothing asserted `source_status` is absent from
the wire when unknown.

## Traps

- **`isPinnedTag` is doing load-bearing work.** If a project ever builds from a
  branch named like a version (`1.2.3`), it will be treated as immutable and never
  checked. Unlikely, and the alternative — asking GitHub about every ref forever —
  costs a call per image per pass for a permanently-identical answer.
- **`branchHead` still exists and is still used** by the package-tag flow. It was
  deliberately not replaced: `/commits/` accepts a tag or SHA, so a mistyped branch
  would silently resolve instead of failing, and failing is safer there.
- **A fleet of `current` proves the pinned-tag path, nothing more.** See above.

## Next step

Nothing in this project depends on phase 5. The first real exercise is a Frigate
custom-branch build: after it, that stack's row should read `build_context: git`,
`source_ref: <branch>`, and `source_status` should flip to `behind` the next time
the branch moves.
