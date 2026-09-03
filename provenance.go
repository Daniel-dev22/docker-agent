package main

// Image provenance: reading back the labels build-agent stamps, so a discovery
// row can answer "what source is this stack running?".
//
// 🔴 THE LABEL KEYS ARE A CROSS-REPO CONTRACT WITH build-agent. Its writer is
// build-agent/provenance.go; these four strings must match it exactly. They are
// deliberately duplicated rather than hoisted into agent-kit-go: the kit is a
// public module and `com.controlcenter.*` is this estate's namespace, so the
// wire-format carve-out applies. Both repos pin the literals in a test, which is
// what makes a change on either side deliberate rather than silent — the failure
// mode otherwise is provenance quietly never appearing, which is indistinguishable
// from "no image has been rebuilt yet".
const (
	labelSource   = "org.opencontainers.image.source"
	labelRevision = "org.opencontainers.image.revision"
	labelRef      = "com.controlcenter.build.ref"
	labelBuildCtx = "com.controlcenter.build.context"
)

// sourceProvenance is what one image says about where it came from. Every field
// is optional; empty means "not known", never "empty value".
type sourceProvenance struct {
	Repo     string
	Ref      string
	Revision string
	Context  string // "git" | "upload"
}

func (p sourceProvenance) empty() bool { return p == sourceProvenance{} }

// provenanceFromLabels reads provenance off an image's OCI labels.
//
// 🔴 It returns NOTHING unless the namespaced build.context label is present, and
// that gate is the whole point. org.opencontainers.image.source and .revision are
// standard labels that a large fraction of third-party images already set —
// anything built by docker/metadata-action does, and ghcr uses .source for repo
// linking. Without the gate an upstream image's own labels would flow into
// discovery, pre-fill the rebuild form, and hand a ref we never built to
// `git clone --branch`. Only build-agent sets com.controlcenter.build.context, so
// it is the one label that means "WE built this".
//
// These labels are not authenticated — any image can claim any of them. The gate
// makes the claim namespaced rather than trusted; consumers must still treat a ref
// as untrusted input (control-api's validBranch does).
func provenanceFromLabels(labels map[string]string) sourceProvenance {
	if labels == nil {
		return sourceProvenance{}
	}
	ctx := labels[labelBuildCtx]
	if ctx != contextGit && ctx != contextUpload {
		return sourceProvenance{}
	}
	return sourceProvenance{
		Repo:     labels[labelSource],
		Ref:      labels[labelRef],
		Revision: labels[labelRevision],
		Context:  ctx,
	}
}

const (
	contextGit    = "git"
	contextUpload = "upload"
)

// provFold folds one provenance field across every container of a compose
// project. A field survives only if EVERY container in the project reported the
// same non-empty value for it.
//
// 🔴 The denominator is the project's container count, and it is the ONLY thing
// enforcing that. A container the slow check pass has not reached yet never folds
// at all, so without comparing seen against the project's own count a stack would
// publish a ref derived from the one service that happened to finish — and the
// answer would then change under the user as the pass completes.
//
// An earlier version also folded unchecked containers as an explicit empty vote.
// That was a second guard for the same invariant, and each one masked the other:
// mutating either left the suite green. One guard, load-bearing.
//
// Fields fold INDEPENDENTLY on purpose: a multi-service stack rebuilt at two
// different commits still has one repo and one ref, and dropping those because the
// revisions differ would discard the only answer the form actually needs.
type provFold struct {
	val      string
	diverged bool
	seen     int
}

func (f *provFold) add(v string) {
	if f.seen > 0 && v != f.val {
		f.diverged = true
	}
	if f.seen == 0 {
		f.val = v
	}
	f.seen++
}

// result returns the folded value, or "" unless all `total` containers
// contributed the same value. An all-empty project needs no separate check: the
// folded value IS the empty string, and a clause testing for it could never
// change the answer.
//
// The comparison is `!=`, not `<`: more votes than the project claims containers
// means the two sources disagree about what the project contains, and a value
// folded over a set we cannot account for is not an answer.
func (f provFold) result(total int) string {
	if f.diverged || f.seen != total {
		return ""
	}
	return f.val
}

// projectProvenance folds a whole project's worth of container provenance.
type projectProvenance struct {
	repo, ref, revision, buildCtx provFold
}

func (a *projectProvenance) add(p sourceProvenance) {
	a.repo.add(p.Repo)
	a.ref.add(p.Ref)
	a.revision.add(p.Revision)
	a.buildCtx.add(p.Context)
}

func (a projectProvenance) result(total int) sourceProvenance {
	return sourceProvenance{
		Repo:     a.repo.result(total),
		Ref:      a.ref.result(total),
		Revision: a.revision.result(total),
		Context:  a.buildCtx.result(total),
	}
}

// builtFromSource is the string the docker-stack-update form's custom-branch
// field is populated from.
//
// 🔴 It is derived from build.context, NOT build.ref. An uploaded-context build
// (genmon) carries no ref at all, so keying on the ref would report every image we
// built ourselves from an uploaded context as "not built by us" — backwards. See
// the phase 1 handoff.
//
// 🔴 It must be the STRING "true", never a JSON bool: the form field is a string
// enum rendered by a string-typed select, and the frontend's populate path assigns
// the attribute verbatim with no coercion.
func builtFromSource(p sourceProvenance) string {
	if p.Context == "" {
		return ""
	}
	return "true"
}
