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
	if got := provenanceFromLabels(upstream); !got.empty() {
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

	if got := provenanceFromLabels(nil); !got.empty() {
		t.Errorf("nil labels => zero; got %+v", got)
	}
	// A value we do not recognise is not a licence to trust the rest of the map.
	bogus := map[string]string{labelBuildCtx: "definitely-not-ours", labelRef: "evil", labelSource: "https://evil.invalid/x.git"}
	if got := provenanceFromLabels(bogus); !got.empty() {
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
	mk := func(project, service, image string) ContainerStatus {
		return ContainerStatus{Name: service, Image: image, ComposeProject: project, ComposeService: service, State: "running"}
	}
	containers := []ContainerStatus{
		mk("frigate", "frigate", "reg/frigate:latest"),
		mk("mixed", "a", "reg/a:latest"),
		mk("mixed", "b", "upstream/b:latest"),
		mk("partial", "p1", "reg/p1:latest"),
		mk("partial", "p2", "reg/p2:latest"),
	}
	projects := []ComposeProject{
		{Name: "frigate", ContainerCount: 1, RunningCount: 1},
		{Name: "mixed", ContainerCount: 2, RunningCount: 2},
		{Name: "partial", ContainerCount: 2, RunningCount: 2},
	}
	ic := &imageChecker{cache: map[string]imageCheck{}}
	ic.cache[compositeKey(containers[0])] = imageCheck{
		ImageStatus: statusUpdated, SourceRepo: "https://github.com/blakeblackshear/frigate.git",
		SourceRef: "0.19", SourceRevision: "deadbeef", BuildContext: contextGit,
	}
	ic.cache[compositeKey(containers[1])] = imageCheck{
		ImageStatus: statusUpdated, SourceRepo: "https://example.invalid/a.git",
		SourceRef: "main", SourceRevision: "aaa", BuildContext: contextGit,
	}
	// containers[2] is a pulled upstream image: cached, checked, no provenance.
	ic.cache[compositeKey(containers[2])] = imageCheck{ImageStatus: statusUpdated}
	// "partial" models a first check pass still in flight: p1 has a result, p2 has
	// none at all. The denominator has to be the project's container count, or the
	// project publishes a ref derived from the one service that happened to finish
	// — and the answer then changes under the user as the pass completes.
	ic.cache[compositeKey(containers[3])] = imageCheck{
		ImageStatus: statusUpdated, SourceRepo: "https://example.invalid/p.git",
		SourceRef: "main", SourceRevision: "ppp", BuildContext: contextGit,
	}

	stampImageStatus(containers, projects, ic)

	if got := containers[0].SourceRef; got != "0.19" {
		t.Errorf("container ref not stamped; got %q", got)
	}
	if got := containers[0].BuiltFromSource; got != "true" {
		t.Errorf("container built_from_source; got %q", got)
	}
	if got := containers[2].BuiltFromSource; got != "" {
		t.Errorf("a pulled image must not claim it was built from source; got %q", got)
	}

	byName := map[string]ComposeProject{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	if got := byName["frigate"]; got.SourceRef != "0.19" || got.SourceRevision != "deadbeef" || got.BuiltFromSource != "true" {
		t.Errorf("single-service project should publish its provenance; got %+v", got)
	}
	// One pulled service means the stack cannot be rebuilt from one ref.
	if got := byName["mixed"]; got.SourceRef != "" || got.SourceRepo != "" || got.BuiltFromSource != "" {
		t.Errorf("mixed stack must publish nothing; got %+v", got)
	}
	if got := byName["partial"]; got.SourceRef != "" || got.SourceRepo != "" || got.BuiltFromSource != "" {
		t.Errorf("an unfinished check pass must publish nothing; got %+v", got)
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

// checkUnit records provenance before it branches on strategy, so a check that
// errors out still carries the source. Driven through checkUnit rather than
// asserted on provenanceFromLabels, because the wiring is the part that breaks.
func TestCheckUnitCarriesProvenanceEvenWhenTheCheckFails(t *testing.T) {
	ic := &imageChecker{overrides: map[string]strategyOverride{}, arch: "amd64"}
	c := ContainerStatus{Name: "frigate", Image: "reg/frigate:latest", ComposeProject: "frigate", ComposeService: "frigate"}
	info := imageInfo{Labels: map[string]string{
		labelSource: "https://github.com/blakeblackshear/frigate.git", labelRef: "0.19",
		labelRevision: "deadbeef", labelBuildCtx: contextGit,
	}}
	// No local RepoDigest and no network: registry-digest fails, which is the
	// point — the source is still known.
	res := ic.checkUnit(t.Context(), c, info)
	if res.Error == "" {
		t.Fatal("expected the check itself to fail, otherwise this proves nothing")
	}
	if res.SourceRef != "0.19" || res.BuildContext != contextGit || res.SourceRevision != "deadbeef" {
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
		Name: "frigate", Image: "reg/frigate:latest", State: "running",
		ComposeProject: "frigate", ComposeService: "frigate",
	}}
	projects := []ComposeProject{{Name: "frigate", ContainerCount: 1, RunningCount: 1}}
	ic := &imageChecker{cache: map[string]imageCheck{
		compositeKey(containers[0]): {
			ImageStatus: statusUpdated, SourceRepo: "https://github.com/blakeblackshear/frigate.git",
			SourceRef: "0.19", SourceRevision: "deadbeef", BuildContext: contextGit,
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
		got.SourceRepo != "https://github.com/blakeblackshear/frigate.git" ||
		got.BuildContext != contextGit || got.BuiltFromSource != "true" {
		t.Errorf("mergeKnown dropped provenance; got %+v", got)
	}
	if s := byName["stopped"]; s.SourceRef != "" || s.BuiltFromSource != "" {
		t.Errorf("a project with no containers must claim nothing; got %+v", s)
	}
}
