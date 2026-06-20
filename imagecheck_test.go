package main

import "testing"

func TestParseImageReference(t *testing.T) {
	cases := []struct {
		in                       string
		reg, repo, tag, fullRepo string
	}{
		{"alpine:latest", "", "alpine", "latest", "alpine"},
		{"alpine", "", "alpine", "latest", "alpine"},
		{"traefik:v3.6.5", "", "traefik", "v3.6.5", "traefik"},
		{"ghcr.io/immich-app/immich-server:release", "ghcr.io", "immich-app/immich-server", "release", "ghcr.io/immich-app/immich-server"},
		{"registry-1.docker.io/library/mariadb:10.6", "registry-1.docker.io", "library/mariadb", "10.6", "registry-1.docker.io/library/mariadb"},
		{"my.registry:5000/team/app:1.2.3", "my.registry:5000", "team/app", "1.2.3", "my.registry:5000/team/app"},
		{"ghcr.io/blakeblackshear/frigate:0.15-abc1234", "ghcr.io", "blakeblackshear/frigate", "0.15-abc1234", "ghcr.io/blakeblackshear/frigate"},
	}
	for _, c := range cases {
		got := parseImageReference(c.in, "latest")
		if got.Registry != c.reg || got.Repository != c.repo || got.Tag != c.tag || got.FullRepo != c.fullRepo {
			t.Errorf("parse %q = {reg:%q repo:%q tag:%q full:%q}, want {%q %q %q %q}",
				c.in, got.Registry, got.Repository, got.Tag, got.FullRepo, c.reg, c.repo, c.tag, c.fullRepo)
		}
	}
}

func TestParseImageReferenceDigest(t *testing.T) {
	got := parseImageReference("alpine@sha256:deadbeef", "latest")
	if got.Digest != "sha256:deadbeef" || got.FullRepo != "alpine" {
		t.Errorf("digest parse = %+v", got)
	}
	if got.String() != "alpine@sha256:deadbeef" {
		t.Errorf("String() = %q", got.String())
	}
}

func TestIsPinnedTag(t *testing.T) {
	pinned := []string{"v3.6.5", "3.6.5", "1.0.0", "v1.2.3-rc1"}
	moving := []string{"latest", "stable", "release", "sts", "alpine", "10.6", "9", "v9", "", "edge"}
	for _, tag := range pinned {
		if !isPinnedTag(tag) {
			t.Errorf("isPinnedTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range moving {
		if isPinnedTag(tag) {
			t.Errorf("isPinnedTag(%q) = true, want false", tag)
		}
	}
}

func TestParseSemverOrdering(t *testing.T) {
	// stable > rc > beta > alpha > dev; numeric ordering within.
	order := []string{"v1.2.3", "1.2.3-rc2", "1.2.3-rc1", "1.2.3-beta1", "1.2.3-alpha1", "1.2.3-dev1"}
	for i := 0; i+1 < len(order); i++ {
		if parseSemver(order[i]).cmp(parseSemver(order[i+1])) <= 0 {
			t.Errorf("%q should sort above %q", order[i], order[i+1])
		}
	}
	if parseSemver("2.0.0").cmp(parseSemver("1.99.99")) <= 0 {
		t.Error("2.0.0 should be > 1.99.99")
	}
	if parseSemver("v3.6.5").cmp(parseSemver("3.6.5")) != 0 {
		t.Error("v-prefix must not change ordering")
	}
}

func TestVersionOutdated(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"1.2.3", "1.2.4", true},
		{"v1.2.3", "1.2.3", false},
		{"1.2.3", "1.2.3", false},
		{"1.2.4", "1.2.3", false},
		{"abc1234", "def5678", true}, // non-semver SHAs → string inequality
		{"abc1234", "abc1234", false},
	}
	for _, c := range cases {
		if got := versionOutdated(c.current, c.latest); got != c.want {
			t.Errorf("versionOutdated(%q,%q)=%v want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestSourceRepoFromLabels(t *testing.T) {
	// The HA case: read the label, don't guess from the image name.
	repo := sourceRepoFromLabels(
		map[string]string{"org.opencontainers.image.source": "https://github.com/home-assistant/core"},
		parseImageReference("ghcr.io/home-assistant/home-assistant:2024.1", "latest"),
	)
	if repo != "home-assistant/core" {
		t.Errorf("HA source = %q, want home-assistant/core", repo)
	}
	// ghcr fallback when no label.
	repo = sourceRepoFromLabels(map[string]string{}, parseImageReference("ghcr.io/owner/pkg:1.0", "latest"))
	if repo != "owner/pkg" {
		t.Errorf("ghcr fallback = %q, want owner/pkg", repo)
	}
	// Docker Hub image, no label → no repo.
	if r := sourceRepoFromLabels(map[string]string{}, parseImageReference("nginx:latest", "latest")); r != "" {
		t.Errorf("hub no-label = %q, want empty", r)
	}
}

func TestAutoDetectStrategy(t *testing.T) {
	ic := &imageChecker{overrides: map[string]strategyOverride{}}
	// pinned + source label → github-release
	s := ic.resolveStrategy(
		ContainerStatus{Image: "traefik:v3.6.5"},
		imageInfo{Labels: map[string]string{"org.opencontainers.image.source": "https://github.com/traefik/traefik"}},
		parseImageReference("traefik:v3.6.5", "latest"),
	)
	if s.source != "github-release" || s.origin != "auto" || s.repo != "traefik/traefik" {
		t.Errorf("traefik auto = %+v, want auto:github-release traefik/traefik", s)
	}
	// moving tag → registry-digest
	s = ic.resolveStrategy(ContainerStatus{Image: "mariadb:10.6"}, imageInfo{}, parseImageReference("mariadb:10.6", "latest"))
	if s.source != "registry-digest" || s.origin != "auto" {
		t.Errorf("mariadb auto = %+v, want auto:registry-digest", s)
	}
	// pinned, no source → registry-digest with flag
	s = ic.resolveStrategy(ContainerStatus{Image: "someapp:1.2.3"}, imageInfo{}, parseImageReference("someapp:1.2.3", "latest"))
	if s.source != "registry-digest" || s.flag != "pinned-no-source" {
		t.Errorf("pinned-no-source = %+v", s)
	}
}

func TestOverrideWins(t *testing.T) {
	ic := &imageChecker{overrides: map[string]strategyOverride{
		"immich": {Key: "immich", MatchType: "project", VersionSource: "github-release", GithubRepo: "immich-app/immich"},
	}}
	s := ic.resolveStrategy(
		ContainerStatus{Image: "ghcr.io/immich-app/immich-server:release", ComposeProject: "immich", ComposeService: "immich-server"},
		imageInfo{},
		parseImageReference("ghcr.io/immich-app/immich-server:release", "latest"),
	)
	if s.origin != "override" || s.source != "github-release" || s.repo != "immich-app/immich" {
		t.Errorf("override = %+v, want override:github-release immich-app/immich", s)
	}
}

func TestTagVariants(t *testing.T) {
	got := tagVariants("v0.16.0")
	want := map[string]bool{"v0.16.0": true, "0.16.0": true, "0.16": true}
	if len(got) != 3 {
		t.Errorf("tagVariants(v0.16.0) = %v", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected variant %q in %v", v, got)
		}
	}
}

func TestFilterDedupeTags(t *testing.T) {
	tags := []string{"abc1234-amd64", "abc1234-arm64", "def5678-amd64", "cache", "h8l-thing", "ci99999-amd64"}
	branchSHAs := map[string]bool{"ci99999": true}
	got := filterDedupeTags(tags, branchSHAs, []string{"cache", "h8l"})
	// abc1234 deduped to one; def5678 kept; cache/h8l excluded; ci99999 is a branch SHA → removed.
	want := []string{"abc1234", "def5678"}
	if len(got) != len(want) {
		t.Fatalf("filterDedupeTags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filterDedupeTags[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
}

func TestParseCurrentTagCustomBranch(t *testing.T) {
	p := parseCurrentTag("0.15-abc1234-amd64", "amd64")
	if !p.custom || p.branch != "0.15" || p.version != "abc1234" {
		t.Errorf("custom branch parse = %+v", p)
	}
	p = parseCurrentTag("abc1234-amd64", "amd64")
	if p.custom || p.version != "abc1234" {
		t.Errorf("plain SHA parse = %+v", p)
	}
}

func TestLocalDigest(t *testing.T) {
	info := imageInfo{RepoDigests: []string{"traefik@sha256:aaaa", "other@sha256:bbbb"}}
	if d := localDigest(info, parseImageReference("traefik:v3", "latest")); d != "sha256:aaaa" {
		t.Errorf("localDigest = %q, want sha256:aaaa", d)
	}
	// library/ normalization
	info = imageInfo{RepoDigests: []string{"library/nginx@sha256:cccc"}}
	if d := localDigest(info, parseImageReference("nginx:latest", "latest")); d != "sha256:cccc" {
		t.Errorf("localDigest library = %q, want sha256:cccc", d)
	}
}
