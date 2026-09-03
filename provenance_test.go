package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The four keys are a contract with build-agent (build-agent/provenance.go).
// Pinned as literals, not derived, so changing one here is a decision someone
// makes on purpose — the failure mode otherwise is provenance silently never
// appearing, which looks exactly like "nothing has been rebuilt yet".
func TestLabelKeysMatchTheBuildAgentContract(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{labelSource, "org.opencontainers.image.source"},
		{labelRevision, "org.opencontainers.image.revision"},
		{labelRef, "com.controlcenter.build.ref"},
		{labelBuildCtx, "com.controlcenter.build.context"},
		{contextGit, "git"},
		{contextUpload, "upload"},
	} {
		if c.got != c.want {
			t.Errorf("label contract drifted: got %q, build-agent writes %q", c.got, c.want)
		}
	}
}

// The gate that keeps a third party's standard OCI labels out of our discovery
// rows. This is the case that actually occurs in production: most upstream images
// built by docker/metadata-action carry .source and .revision.
func TestProvenanceFromLabelsIgnoresUpstreamOCILabels(t *testing.T) {
	upstream := map[string]string{
		"org.opencontainers.image.source":   "https://github.com/blakeblackshear/frigate",
		"org.opencontainers.image.revision": "1a2b3c4d",
		"org.opencontainers.image.version":  "0.19.0",
		"org.opencontainers.image.title":    "frigate",
	}
	// Asserted field by field, not via empty(): review showed making empty()
	// return true unconditionally left this — "the most important line in the
	// phase" — passing.
	got := provenanceFromLabels(upstream)
	if got.Repo != "" || got.Ref != "" || got.Revision != "" || got.Context != "" {
		t.Fatalf("an image we did not build must report no provenance; got %+v", got)
	}
}

func TestProvenanceFromLabels(t *testing.T) {
	ours := map[string]string{
		labelSource:   "https://github.com/blakeblackshear/frigate.git",
		labelRevision: "deadbeef",
		labelRef:      "0.19",
		labelBuildCtx: "git",
	}
	got := provenanceFromLabels(ours)
	want := sourceProvenance{Repo: "https://github.com/blakeblackshear/frigate.git", Ref: "0.19", Revision: "deadbeef", Context: "git"}
	if got != want {
		t.Errorf("our own labels\n got %+v\nwant %+v", got, want)
	}

	// An uploaded-context build carries only the context label — that is exactly
	// why the context label exists.
	up := provenanceFromLabels(map[string]string{labelBuildCtx: "upload"})
	if up != (sourceProvenance{Context: "upload"}) {
		t.Errorf("upload build => context only; got %+v", up)
	}

	if got := provenanceFromLabels(nil); got != (sourceProvenance{}) {
		t.Errorf("nil labels => zero; got %+v", got)
	}
	// A value we do not recognise is not a licence to trust the rest of the map.
	bogus := map[string]string{labelBuildCtx: "definitely-not-ours", labelRef: "evil", labelSource: "https://evil.invalid/x.git"}
	if got := provenanceFromLabels(bogus); got != (sourceProvenance{}) {
		t.Errorf("unrecognised context => zero; got %+v", got)
	}
}

func TestBuiltFromSourceIsAQuotedString(t *testing.T) {
	if got := builtFromSource(sourceProvenance{}); got != "" {
		t.Errorf("no context => absent; got %q", got)
	}
	for _, ctx := range []string{contextGit, contextUpload} {
		if got := builtFromSource(sourceProvenance{Context: ctx}); got != "true" {
			t.Errorf("%s => \"true\"; got %q", ctx, got)
		}
	}
	// The form field is a string enum and the frontend assigns the attribute with
	// no coercion, so a JSON bool would land `true` in a string-typed select.
	b, err := json.Marshal(ComposeProject{Name: "x", BuiltFromSource: builtFromSource(sourceProvenance{Context: contextGit})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"built_from_source":"true"`) {
		t.Errorf("built_from_source must serialise as a quoted string; got %s", b)
	}
	if strings.Contains(string(b), `"built_from_source":true`) {
		t.Errorf("built_from_source serialised as a bool; got %s", b)
	}
}

func TestProjectProvenanceUnanimity(t *testing.T) {
	git := func(repo, ref, rev string) sourceProvenance {
		return sourceProvenance{Repo: repo, Ref: ref, Revision: rev, Context: contextGit}
	}
	cases := []struct {
		name  string
		in    []sourceProvenance
		total int
		want  sourceProvenance
	}{
		{
			name: "all agree", total: 2,
			in:   []sourceProvenance{git("r", "0.19", "abc"), git("r", "0.19", "abc")},
			want: git("r", "0.19", "abc"),
		},
		{
			name: "one service on a different ref drops ref, keeps repo", total: 2,
			in:   []sourceProvenance{git("r", "0.19", "abc"), git("r", "dev", "abc")},
			want: sourceProvenance{Repo: "r", Revision: "abc", Context: contextGit},
		},
		{
			name: "rebuilt at two commits keeps repo and ref, drops revision", total: 2,
			in:   []sourceProvenance{git("r", "0.19", "abc"), git("r", "0.19", "def")},
			want: sourceProvenance{Repo: "r", Ref: "0.19", Context: contextGit},
		},
		{
			name: "one pulled sidecar makes the whole stack unknown", total: 2,
			in:   []sourceProvenance{git("r", "0.19", "abc"), {}},
			want: sourceProvenance{},
		},
		{
			name: "an unchecked container is not unanimity", total: 3,
			in:   []sourceProvenance{git("r", "0.19", "abc"), git("r", "0.19", "abc")},
			want: sourceProvenance{},
		},
		{
			name: "a project with no containers claims nothing", total: 0,
			in: nil, want: sourceProvenance{},
		},
		{
			// More votes than the project claims containers: the container list and
			// the project's own count disagree, so the fold covers a set we cannot
			// account for. Distinguishes `!=` from `<`.
			name: "more votes than containers claims nothing", total: 1,
			in:   []sourceProvenance{git("r", "0.19", "abc"), git("r", "0.19", "abc")},
			want: sourceProvenance{},
		},
		{
			name: "every container checked, none built by us", total: 2,
			in:   []sourceProvenance{{}, {}},
			want: sourceProvenance{},
		},
	}
	for _, c := range cases {
		var a projectProvenance
		for _, p := range c.in {
			a.add(p)
		}
		if got := a.result(c.total); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

// The composition layer: the store (imageChecker cache) and the row builders are
// each covered above, but what actually reaches the wire is assembled here — and
// that is the layer a phase's tests most often skip entirely.
func TestStampImageStatusPublishesProvenance(t *testing.T) {
	mk := func(project, service, image, imageID string) ContainerStatus {
		return ContainerStatus{Name: service, Image: image, ImageID: imageID,
			ComposeProject: project, ComposeService: service, State: "running"}
	}
	containers := []ContainerStatus{
		mk("frigate", "frigate", "reg/frigate:latest", "sha256:aaa"),
		mk("mixed", "a", "reg/a:latest", "sha256:bbb"),
		mk("mixed", "b", "upstream/b:latest", "sha256:ccc"),
		mk("partial", "p1", "reg/p1:latest", "sha256:ddd"),
		mk("partial", "p2", "reg/p2:latest", "sha256:eee"),
		mk("genmon", "genmon", "reg/genmon:1.4.0", "sha256:fff"),
		mk("moved", "moved", "reg/moved:latest", "sha256:NEW"),
	}
	projects := []ComposeProject{
		{Name: "frigate", ContainerCount: 1, RunningCount: 1},
		// RunningCount deliberately differs from ContainerCount everywhere below:
		// review showed every fixture had them equal, so the fold could be pointed
		// at RunningCount — the wrong denominator — with the suite green.
		{Name: "mixed", ContainerCount: 2, RunningCount: 1},
		{Name: "partial", ContainerCount: 2, RunningCount: 1},
		{Name: "genmon", ContainerCount: 1, RunningCount: 1},
		{Name: "moved", ContainerCount: 1, RunningCount: 1},
	}
	ic := &imageChecker{cache: map[string]imageCheck{}}
	ic.cache[compositeKey(containers[0])] = imageCheck{
		ImageStatus: statusUpdated, ImageID: "sha256:aaa",
		SourceRepo: "https://github.com/blakeblackshear/frigate",
		SourceRef:  "0.19", SourceRevision: "deadbeef", BuildContext: contextGit,
	}
	ic.cache[compositeKey(containers[1])] = imageCheck{
		ImageStatus: statusUpdated, ImageID: "sha256:bbb",
		SourceRepo: "https://example.invalid/a", SourceRef: "main",
		SourceRevision: "aaa", BuildContext: contextGit,
	}
	ic.cache[compositeKey(containers[2])] = imageCheck{ImageStatus: statusUpdated, ImageID: "sha256:ccc"}
	ic.cache[compositeKey(containers[3])] = imageCheck{
		ImageStatus: statusUpdated, ImageID: "sha256:ddd",
		SourceRepo: "https://example.invalid/p", SourceRef: "main",
		SourceRevision: "ppp", BuildContext: contextGit,
	}
	// containers[4] ("p2") has NO cache entry: the first check pass is still running.
	// genmon: an uploaded-context build — built by us, with no ref. This is the
	// case the built_from_source rule exists for, and no fixture had it.
	ic.cache[compositeKey(containers[5])] = imageCheck{
		ImageStatus: statusUpdated, ImageID: "sha256:fff", BuildContext: contextUpload,
	}
	// "moved": the cached result describes the image this container USED to run.
	ic.cache[compositeKey(containers[6])] = imageCheck{
		ImageStatus: statusUpdated, ImageID: "sha256:OLD",
		SourceRepo: "https://example.invalid/m", SourceRef: "main",
		SourceRevision: "stale", BuildContext: contextGit,
	}

	stampImageStatus(containers, projects, ic)

	// Full equality on every provenance field of the container, not just one:
	// review showed SourceRepo, SourceRevision and BuildContext were written but
	// never read, so all three could be dropped at the stamp site with the suite green.
	want := ContainerStatus{
		SourceRepo: "https://github.com/blakeblackshear/frigate", SourceRef: "0.19",
		SourceRevision: "deadbeef", BuildContext: contextGit,
		BuiltFromSource: "true", RebuildableFromRef: "true",
	}
	got := containers[0]
	if got.SourceRepo != want.SourceRepo || got.SourceRef != want.SourceRef ||
		got.SourceRevision != want.SourceRevision || got.BuildContext != want.BuildContext ||
		got.BuiltFromSource != want.BuiltFromSource || got.RebuildableFromRef != want.RebuildableFromRef {
		t.Errorf("container provenance\n got %+v\nwant %+v", got, want)
	}
	// A pulled upstream image claims nothing, on every field.
	for _, f := range []string{containers[2].SourceRepo, containers[2].SourceRef,
		containers[2].SourceRevision, containers[2].BuildContext,
		containers[2].BuiltFromSource, containers[2].RebuildableFromRef} {
		if f != "" {
			t.Errorf("a pulled image must claim nothing; got %+v", containers[2])
			break
		}
	}
	// An unchecked container must be given nothing — not a fabricated "true".
	for _, f := range []string{containers[4].SourceRef, containers[4].BuiltFromSource, containers[4].RebuildableFromRef} {
		if f != "" {
			t.Errorf("an unchecked container must claim nothing; got %+v", containers[4])
			break
		}
	}
	// genmon: built by us, but not rebuildable from a ref. Both call sites.
	if containers[5].BuiltFromSource != "true" {
		t.Errorf("an upload-context build IS built from source; got %q", containers[5].BuiltFromSource)
	}
	if containers[5].RebuildableFromRef != "" {
		t.Errorf("an upload build has no ref, so it is not rebuildable from one; got %q", containers[5].RebuildableFromRef)
	}
	// A moved tag must not publish the previous image's commit.
	if containers[6].SourceRevision != "" || containers[6].BuiltFromSource != "" {
		t.Errorf("a stale cache entry must contribute nothing; got %+v", containers[6])
	}

	byName := map[string]ComposeProject{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	if g := byName["frigate"]; g.SourceRef != "0.19" || g.SourceRevision != "deadbeef" ||
		g.SourceRepo != "https://github.com/blakeblackshear/frigate" || g.BuildContext != contextGit ||
		g.BuiltFromSource != "true" || g.RebuildableFromRef != "true" {
		t.Errorf("single-service project should publish its provenance; got %+v", g)
	}
	if g := byName["mixed"]; g.SourceRef != "" || g.SourceRepo != "" || g.BuiltFromSource != "" {
		t.Errorf("mixed stack must publish nothing; got %+v", g)
	}
	if g := byName["partial"]; g.SourceRef != "" || g.SourceRepo != "" || g.BuiltFromSource != "" {
		t.Errorf("an unfinished check pass must publish nothing; got %+v", g)
	}
	if g := byName["genmon"]; g.BuiltFromSource != "true" || g.RebuildableFromRef != "" || g.SourceRef != "" {
		t.Errorf("upload project: built, not rebuildable; got %+v", g)
	}
	if g := byName["moved"]; g.SourceRevision != "" || g.BuiltFromSource != "" {
		t.Errorf("moved-tag project must publish nothing; got %+v", g)
	}
}

func TestProvenanceOfRejectsAStaleCacheEntry(t *testing.T) {
	full := imageCheck{ImageID: "sha256:new", SourceRef: "0.19", BuildContext: contextGit}
	if got := provenanceOf(full, ContainerStatus{ImageID: "sha256:new"}); got.Ref != "0.19" {
		t.Errorf("matching image id must yield provenance; got %+v", got)
	}
	for _, c := range []struct {
		name string
		r    imageCheck
		cs   ContainerStatus
	}{
		{"different content", full, ContainerStatus{ImageID: "sha256:old"}},
		{"cache entry has no id", imageCheck{SourceRef: "0.19", BuildContext: contextGit}, ContainerStatus{ImageID: "sha256:new"}},
		{"container has no id", full, ContainerStatus{}},
	} {
		if got := provenanceOf(c.r, c.cs); got != (sourceProvenance{}) {
			t.Errorf("%s must contribute nothing; got %+v", c.name, got)
		}
	}
}

func TestRebuildableFromRef(t *testing.T) {
	cases := []struct {
		in        sourceProvenance
		want, why string
	}{
		{sourceProvenance{Context: contextGit, Ref: "0.19"}, "true", "git build with a ref"},
		{sourceProvenance{Context: contextGit}, "", "git build with no ref — default branch, nothing to pre-fill"},
		{sourceProvenance{Context: contextUpload}, "", "an uploaded tar has no ref"},
		{sourceProvenance{}, "", "no provenance"},
	}
	for _, c := range cases {
		if got := rebuildableFromRef(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.why, got, c.want)
		}
	}
	// The invariant the rebuild form depends on: never a toggle without a branch.
	for _, p := range []sourceProvenance{
		{Context: contextGit, Ref: "0.19"}, {Context: contextGit}, {Context: contextUpload}, {},
	} {
		if rebuildableFromRef(p) == "true" && p.Ref == "" {
			t.Errorf("rebuildable must imply a ref exists; %+v", p)
		}
	}
}

// Values come off whatever image is on this host, and are re-published into a
// JSONB column and every fleet frame. The writer's sanitising happens in another
// process on another host and is not a property of what we read.
func TestProvenanceFromLabelsSanitizesWhatItRepublishes(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		m := map[string]string{labelBuildCtx: contextGit}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct{ name, key, val string }{
		{"NUL breaks a jsonb insert", labelRef, "0.19\x00evil"},
		{"newline", labelRef, "0.19\nx"},
		{"over the cap", labelRef, strings.Repeat("a", maxLabelValue+1)},
		{"C1 control", labelRef, "0.19\u0085x"},
		{"bidi override", labelRef, "0.19\u202ex"},
		{"invalid UTF-8", labelRef, "0.19\xff\xfe"},
		{"over-cap repo", labelSource, strings.Repeat("b", maxLabelValue+1)},
		{"newline in revision", labelRevision, "abc\ndef"},
	} {
		got := provenanceFromLabels(base(map[string]string{c.key: c.val}))
		var field string
		switch c.key {
		case labelRef:
			field = got.Ref
		case labelSource:
			field = got.Repo
		case labelRevision:
			field = got.Revision
		}
		if field != "" {
			t.Errorf("%s: republished %q", c.name, field)
		}
		// The context label is ours and still valid, so the row stays identifiable.
		if got.Context != contextGit {
			t.Errorf("%s: context must survive a bad sibling field", c.name)
		}
	}
	if got := provenanceFromLabels(base(map[string]string{labelRef: strings.Repeat("a", maxLabelValue)})); got.Ref == "" {
		t.Error("a value exactly at the cap must pass")
	}
	if maxLabelValue != 512 {
		t.Errorf("read cap is %d; it mirrors build-agent's writer cap — change both deliberately", maxLabelValue)
	}
}

// sourceRepoFromLabels is the OTHER reader of org.opencontainers.image.source and
// it picks an update strategy. Two lenses found that phase 1 began writing a value
// it could not parse.
func TestSourceRepoFromLabelsRejectsCloneURLs(t *testing.T) {
	ref := imageRef{}
	cases := []struct{ in, want, why string }{
		{"https://github.com/o/r", "o/r", "conformant URL"},
		{"https://github.com/o/r.git", "o/r", ".git stripped — it 404s every release lookup otherwise"},
		{"git@github.com:o/r.git", "", "scp syntax has exactly one slash and must NOT read as a slug"},
		{"git@github.com:Daniel-dev22/build-agent.git", "", "the estate's configured form"},
		{"ssh://git@github.com/o/r.git", "", "ssh URL is not a slug"},
		{"o/r", "o/r", "already a slug"},
		{"", "", "absent"},
	}
	for _, c := range cases {
		if got := sourceRepoFromLabels(map[string]string{"org.opencontainers.image.source": c.in}, ref); got != c.want {
			t.Errorf("%s: sourceRepoFromLabels(%q) = %q, want %q", c.why, c.in, got, c.want)
		}
	}
	// What build-agent now emits must round-trip to a clean slug.
	if got := sourceRepoFromLabels(map[string]string{"org.opencontainers.image.source": "https://github.com/Daniel-dev22/build-agent"}, ref); got != "Daniel-dev22/build-agent" {
		t.Errorf("build-agent's emitted form must parse; got %q", got)
	}
}

// Every provenance field must vanish from the JSON when unknown. An empty
// source_ref downstream reads as "a branch named empty string", and an empty
// built_from_source would defeat the form's fall-back to the manual path.
func TestUnknownProvenanceIsAbsentFromTheWireNotEmpty(t *testing.T) {
	for _, v := range []any{
		ComposeProject{Name: "x", ContainerCount: 1},
		ContainerStatus{Name: "x", Image: "y"},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"source_repo", "source_ref", "source_revision", "build_context", "built_from_source"} {
			if strings.Contains(string(b), key) {
				t.Errorf("%T: %s must be absent when unknown, got %s", v, key, b)
			}
		}
	}
}

// Four of the five wire names were free to change: the absence test passes for a
// renamed tag too. Phase 3 reads `source_ref` by name, and a rename would make it
// read nothing — indistinguishable from "nothing has been rebuilt yet".
func TestProvenanceWireNames(t *testing.T) {
	cp, err := json.Marshal(ComposeProject{
		Name: "x", ContainerCount: 1,
		SourceRepo: "https://github.com/o/r", SourceRef: "0.19", SourceRevision: "abc",
		BuildContext: contextGit, BuiltFromSource: "true", RebuildableFromRef: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := json.Marshal(ContainerStatus{
		Name: "x", Image: "y",
		SourceRepo: "https://github.com/o/r", SourceRef: "0.19", SourceRevision: "abc",
		BuildContext: contextGit, BuiltFromSource: "true", RebuildableFromRef: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"source_repo":"https://github.com/o/r"`,
		`"source_ref":"0.19"`,
		`"source_revision":"abc"`,
		`"build_context":"git"`,
		`"built_from_source":"true"`,
		`"rebuildable_from_ref":"true"`,
	} {
		if !strings.Contains(string(cp), want) {
			t.Errorf("ComposeProject wire: missing %s in %s", want, cp)
		}
		if !strings.Contains(string(cs), want) {
			t.Errorf("ContainerStatus wire: missing %s in %s", want, cs)
		}
	}
}

// checkUnit records provenance before it branches on strategy, so a check that
// errors out still carries the source. Driven through checkUnit rather than
// asserted on provenanceFromLabels, because the wiring is the part that breaks.
func TestCheckUnitCarriesProvenanceEvenWhenTheCheckFails(t *testing.T) {
	ic := &imageChecker{overrides: map[string]strategyOverride{}, arch: "amd64"}
	c := ContainerStatus{Name: "frigate", Image: "reg/frigate:latest", ComposeProject: "frigate", ComposeService: "frigate"}
	info := imageInfo{Labels: map[string]string{
		labelSource: "https://github.com/blakeblackshear/frigate", labelRef: "0.19",
		labelRevision: "deadbeef", labelBuildCtx: contextGit,
	}}
	// No local RepoDigest and no network: registry-digest fails, which is the
	// point — the source is still known.
	res := ic.checkUnit(t.Context(), c, info)
	if res.Error == "" {
		t.Fatal("expected the check itself to fail, otherwise this proves nothing")
	}
	if res.SourceRef != "0.19" || res.BuildContext != contextGit ||
		res.SourceRevision != "deadbeef" || res.SourceRepo != "https://github.com/blakeblackshear/frigate" {
		t.Errorf("a failed check must still carry provenance; got %+v", res)
	}
}

// mergeKnown runs AFTER stampImageStatus in the snapshot build, and it is the
// last thing to touch the project rows before they reach both the fleet frame and
// the discovery push. It mutates matched entries in place today, but a rebuild of
// the struct there would silently drop everything this phase adds — with every
// other test in this file still green, because none of them run this step.
func TestProvenanceSurvivesTheMergeKnownStep(t *testing.T) {
	containers := []ContainerStatus{{
		Name: "frigate", Image: "reg/frigate:latest", ImageID: "sha256:aaa", State: "running",
		ComposeProject: "frigate", ComposeService: "frigate",
	}}
	projects := []ComposeProject{{Name: "frigate", ContainerCount: 1, RunningCount: 1}}
	ic := &imageChecker{cache: map[string]imageCheck{
		compositeKey(containers[0]): {
			ImageStatus: statusUpdated, ImageID: "sha256:aaa",
			SourceRepo: "https://github.com/blakeblackshear/frigate",
			SourceRef:  "0.19", SourceRevision: "deadbeef", BuildContext: contextGit,
		},
	}}
	stampImageStatus(containers, projects, ic)

	reg := newComposeRegistry("", "/data")
	reg.byName["frigate"] = &ProjectEntry{Name: "frigate", WorkingDir: "/data/frigate"}
	// A stopped-but-registered project with no live containers: it must appear,
	// and it must claim no provenance rather than inheriting anyone else's.
	reg.byName["stopped"] = &ProjectEntry{Name: "stopped", WorkingDir: "/data/stopped"}

	merged := reg.mergeKnown(projects)

	byName := map[string]ComposeProject{}
	for _, p := range merged {
		byName[p.Name] = p
	}
	got, ok := byName["frigate"]
	if !ok {
		t.Fatal("frigate vanished from the merged projects")
	}
	if got.SourceRef != "0.19" || got.SourceRevision != "deadbeef" ||
		got.SourceRepo != "https://github.com/blakeblackshear/frigate" ||
		got.BuildContext != contextGit || got.BuiltFromSource != "true" ||
		got.RebuildableFromRef != "true" {
		t.Errorf("mergeKnown dropped provenance; got %+v", got)
	}
	if s := byName["stopped"]; s.SourceRef != "" || s.BuiltFromSource != "" || s.RebuildableFromRef != "" {
		t.Errorf("a project with no containers must claim nothing; got %+v", s)
	}
}
