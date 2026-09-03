package main

import (
	"strings"
	"unicode/utf8"
)

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
		Repo:     sanitizeLabelValue(labels[labelSource]),
		Ref:      sanitizeLabelValue(labels[labelRef]),
		Revision: sanitizeLabelValue(labels[labelRevision]),
		Context:  ctx,
	}
}

// Label values are re-published: onto every fleet websocket frame, and into the
// controller's discovery row, which the router inserts into a JSONB column.
//
// 🔴 THE WRITER'S SANITISING IS NOT A SYSTEM PROPERTY. build-agent bounds what it
// writes, but it runs in a different process on a different host, and these labels
// come off whatever image is on THIS host — the gate above makes the claim
// namespaced, not trusted. An invariant the reader does not re-establish is not an
// invariant.
//
// Concretely: the container inserts share one transaction with a DELETE, so a
// single value PostgreSQL cannot store in jsonb (a NUL is the cheap case) fails
// the whole push and freezes that node's docker discovery at its last good
// snapshot, with a 500 in a log as the only symptom. An unbounded value needs no
// crafting at all — it just inflates every frame and every row, forever.
//
// Same rules as the writer, deliberately: drop rather than repair, because a
// silently-edited ref is a wrong answer and being trusted is this value's only job.
const maxLabelValue = 512

func sanitizeLabelValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxLabelValue || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		switch {
		case r < 0x20, r == 0x7f:
			return ""
		case r >= 0x80 && r <= 0x9f: // C1
			return ""
		case r == 0x2028 || r == 0x2029: // line / paragraph separator
			return ""
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi
			return ""
		case r == 0xfeff:
			return ""
		}
	}
	return s
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

// rebuildableFromRef answers the question the rebuild FORM asks, which is not the
// question builtFromSource answers.
//
// Review found the two had been conflated: an uploaded-context build (genmon) and
// a git build with no explicit ref both yield built_from_source="true" with
// source_ref structurally absent. Phase 3 maps the custom-branch toggle from one
// field and the branch name from the other, so those stacks would pre-fill a form
// that says "build a custom branch" with no branch — self-contradictory, and with
// no explanation available to whoever is looking at it.
//
// So: built_from_source stays honest about origin ("we built this"), and this
// field is what the toggle keys on. Both are present together or not at all.
func rebuildableFromRef(p sourceProvenance) string {
	if p.Context != contextGit || p.Ref == "" {
		return ""
	}
	return "true"
}

// provenanceOf returns the provenance a cached check may contribute for one
// container, or the zero value if the cache entry describes a different image.
//
// compositeKey is (project, service, image REFERENCE) and deliberately excludes
// the image ID, because every other field on imageCheck is a status about that
// reference. Provenance is not: it is a claim about content. A redeploy that
// leaves the reference unchanged (`compose pull && up -d` on :latest, which is how
// ansible ships every first-party image) keeps the key valid while the content
// underneath it changes, and the stale commit would then be published as the
// project's unanimous revision.
func provenanceOf(r imageCheck, c ContainerStatus) sourceProvenance {
	if r.ImageID == "" || c.ImageID == "" || r.ImageID != c.ImageID {
		return sourceProvenance{}
	}
	return sourceProvenance{Repo: r.SourceRepo, Ref: r.SourceRef, Revision: r.SourceRevision, Context: r.BuildContext}
}
