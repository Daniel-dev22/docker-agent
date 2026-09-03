package main

// GitHub version sources — two lookups behind one client:
//
//   - githubClient.latestRelease — for an image versioned by its project's GitHub
//     releases: semver sort, prerelease/draft filter, name_filter, tag_exclude.
//   - githubClient.latestPackageVersion — for an image versioned by a GitHub
//     Packages tag stream keyed off a branch's CI builds: package versions minus
//     the excluded branch's commit SHAs, the tracked branch head, and a
//     registry-existence verify of each candidate.
//
// GitHub auth is a GitHub App installation token vended by the controller
// (POST /api/docker/github-token) — the agent holds no App private key.
// Unauthenticated GitHub is 60 requests/hr, so a whole fleet would 429
// immediately and a token is required. The token is cached per agent; a 300s
// result TTL + bounded concurrency (imagecheck.go) keep a fleet far under the
// authenticated ceiling.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	githubAPIBase   = "https://api.github.com"
	githubUserAgent = "docker-agent-imagecheck/1.0"
)

// ---------------------------------------------------------------------------
// Token vend (agent → controller).
// ---------------------------------------------------------------------------

type githubTokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func readErrorBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 2048))
	return strings.TrimSpace(string(b))
}

// githubClient mints + caches an installation token and queries the GitHub API
// directly (public internet egress; NOT through the controller dial-rewriter —
// that path is only for the controller). registry is shared for the existence gate.
type githubClient struct {
	cc       *http.Client // controller client (bearer + dial rewrite) — token vend only
	ccURL    string
	api      *http.Client // plain pooled client for api.github.com
	apiBase  string
	registry *registryClient

	mu       sync.Mutex
	token    string
	tokenExp time.Time

	// composeCache memoizes upstream compose bodies by resolved URL. A resolved
	// URL embeds the release tag, and a tag's compose file is immutable, so a hit
	// is always correct and a new release simply mints a new key. In-memory is the
	// right layer: on restart we re-query GitHub (the source of truth) once, and
	// nothing is replayed, re-fired, or lost.
	composeMu    sync.Mutex
	composeCache map[string]map[string]string // url → service → image TEMPLATE (pre-interpolation)
}

func newGithubClient(cc *http.Client, ccURL string, registry *registryClient) *githubClient {
	return &githubClient{
		cc:           cc,
		ccURL:        ccURL,
		api:          &http.Client{Timeout: 30 * time.Second, Transport: pooledTransport(false)},
		apiBase:      githubAPIBase,
		registry:     registry,
		composeCache: map[string]map[string]string{},
	}
}

// ensureToken returns a valid installation token, vending a fresh one through the
// controller when the cache is empty or within 5 minutes of expiry.
func (g *githubClient) ensureToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	if g.token != "" && time.Now().Before(g.tokenExp.Add(-5*time.Minute)) {
		tok := g.token
		g.mu.Unlock()
		return tok, nil
	}
	g.mu.Unlock()

	if g.cc == nil || g.ccURL == "" {
		return "", fmt.Errorf("controller client/url not configured")
	}
	body, _ := json.Marshal(map[string]any{"repositories": []string{}})
	url := g.ccURL + "/api/docker/github-token"

	const maxAttempts = 4
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("token vend ctx done: %w", ctx.Err())
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		tok, transient, err := g.vendOnce(ctx, url, body)
		if err == nil {
			g.mu.Lock()
			g.token, g.tokenExp = tok.Token, tok.ExpiresAt
			g.mu.Unlock()
			return tok.Token, nil
		}
		if !transient {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("github-token vend failed after %d attempts: %w", maxAttempts, lastErr)
}

func (g *githubClient) vendOnce(ctx context.Context, url string, body []byte) (githubTokenResp, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return githubTokenResp{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.cc.Do(req)
	if err != nil {
		return githubTokenResp{}, true, fmt.Errorf("call controller: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return githubTokenResp{}, false, fmt.Errorf("controller rejected token vend (%d): %s", resp.StatusCode, readErrorBody(resp.Body))
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return githubTokenResp{}, true, fmt.Errorf("controller transient %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	default:
		return githubTokenResp{}, false, fmt.Errorf("controller returned %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}
	var t githubTokenResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return githubTokenResp{}, false, fmt.Errorf("decode vend response: %w", err)
	}
	if t.Token == "" {
		return githubTokenResp{}, false, fmt.Errorf("vend response missing token")
	}
	return t, false, nil
}

// apiGet performs an authenticated GitHub API GET, returning the decoded body and
// the raw response (for the Link header on paginated calls).
func (g *githubClient) apiGet(ctx context.Context, url string, out any) (*http.Response, error) {
	token, err := g.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", githubUserAgent)
	resp, err := g.api.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body := readErrorBody(resp.Body)
		resp.Body.Close()
		return resp, fmt.Errorf("GitHub %d: %s", resp.StatusCode, body)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
			resp.Body.Close()
			return resp, fmt.Errorf("decode GitHub response: %w", err)
		}
	}
	resp.Body.Close()
	return resp, nil
}

// ---------------------------------------------------------------------------
// Raw file fetch (retrying, pooled, optionally authenticated).
//
// The compose body behind the upstream-compose resolver used to be pulled
// anonymously from raw.githubusercontent.com in a single un-retried GET. That
// host is Fastly-fronted with a bursty anonymous per-IP limit, so one 429 killed
// a whole immich update — while the latestRelease call one line earlier was
// already authenticated. rawContents closes that gap: same installation token,
// same ~5k/hr budget.
// ---------------------------------------------------------------------------

const (
	fetchMaxAttempts = 4
	fetchBackoffCap  = 4 * time.Second
	fetchMaxBytes    = 4 << 20
)

// fetchBackoffBase is the first retry delay. A var, not a const, so tests can
// shrink it without sleeping for seconds.
var fetchBackoffBase = 500 * time.Millisecond

// retryAfter parses a Retry-After header (delta-seconds or HTTP-date). It returns
// 0 when the header is absent, malformed, or already in the past. GitHub's raw
// 429s carry no Retry-After at all, so the caller must not depend on it.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	if d := time.Until(t); d > 0 {
		return d
	}
	return 0
}

// getOnce performs a single GET. transient reports whether a retry could plausibly
// succeed: 429, 5xx, and network errors are transient; every other 4xx is fatal,
// because retrying a 404 or a bad credential only burns budget.
func (g *githubClient) getOnce(ctx context.Context, u, accept string, auth bool) (body []byte, retry time.Duration, transient bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, false, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", githubUserAgent)
	if auth {
		token, terr := g.ensureToken(ctx)
		if terr != nil {
			return nil, 0, false, terr
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := g.api.Do(req)
	if err != nil {
		return nil, 0, true, fmt.Errorf("get %s: %w", u, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, retryAfter(resp.Header), true, fmt.Errorf("fetch %s: HTTP %d", u, resp.StatusCode)
	default:
		return nil, 0, false, fmt.Errorf("fetch %s: HTTP %d: %s", u, resp.StatusCode, readErrorBody(resp.Body))
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if rerr != nil {
		return nil, 0, true, fmt.Errorf("read %s: %w", u, rerr)
	}
	return b, 0, false, nil
}

// nextWait is the delay before the next attempt: the exponential backoff, capped,
// but never shorter than a server-supplied Retry-After. The caller's context
// bounds the total, so an absurd Retry-After stalls nothing.
func nextWait(backoff, retry time.Duration) time.Duration {
	wait := min(backoff, fetchBackoffCap)
	if retry > wait {
		wait = retry
	}
	return wait
}

// getBytes GETs u with the shared pooled client, retrying transient failures with
// exponential backoff that defers to Retry-After when the server sends one.
func (g *githubClient) getBytes(ctx context.Context, u, accept string, auth bool) ([]byte, error) {
	backoff := fetchBackoffBase
	var lastErr error
	for attempt := 1; attempt <= fetchMaxAttempts; attempt++ {
		body, retry, transient, err := g.getOnce(ctx, u, accept, auth)
		if err == nil {
			return body, nil
		}
		if !transient {
			return nil, err
		}
		lastErr = err
		if attempt == fetchMaxAttempts {
			break
		}
		wait := nextWait(backoff, retry)
		slog.Warn("github fetch transient, retrying",
			"url", u, "attempt", attempt, "of", fetchMaxAttempts, "wait", wait, "error", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("fetch %s: %w", u, ctx.Err())
		case <-time.After(wait):
		}
		backoff *= 2
	}
	return nil, fmt.Errorf("%w (after %d attempts)", lastErr, fetchMaxAttempts)
}

// rawContents fetches a repository file's raw bytes through the AUTHENTICATED
// Contents API rather than raw.githubusercontent.com. Same bytes, but it rides
// the installation token's budget instead of the anonymous per-IP bucket.
func (g *githubClient) rawContents(ctx context.Context, repo, path, ref string) ([]byte, error) {
	u := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", g.apiBase, repo, path, url.QueryEscape(ref))
	return g.getBytes(ctx, u, "application/vnd.github.raw", true)
}

// ---------------------------------------------------------------------------
// Semver (port of github_releases.parse_semver).
// ---------------------------------------------------------------------------

var semverRe = regexp.MustCompile(`(?i)^(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:[-.]?(alpha|beta|rc|dev)[-.]?(\d+)?)?`)

// semver is (major, minor, patch, prereleasePriority, prereleaseNum). A stable
// release sorts above any prerelease (priority 5); dev<alpha<beta<rc<stable.
type semver [5]int

func parseSemver(v string) semver {
	if v == "" {
		return semver{}
	}
	v = strings.TrimLeft(v, "vV")
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return semver{}
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	prio := 5 // stable
	if m[4] != "" {
		switch strings.ToLower(m[4]) {
		case "dev":
			prio = 1
		case "alpha":
			prio = 2
		case "beta":
			prio = 3
		case "rc":
			prio = 4
		default:
			prio = 0
		}
	}
	return semver{atoi(m[1]), atoi(m[2]), atoi(m[3]), prio, atoi(m[5])}
}

// cmp returns -1/0/1 comparing a to b.
func (a semver) cmp(b semver) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// github-release source — "what is the newest release tag of this repo?".
// ---------------------------------------------------------------------------

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// releaseQuery configures a github-release lookup.
type releaseQuery struct {
	Repo        string // owner/repo
	ReleaseType string // "latest" (default) | "stable"
	NameFilter  string // substring a release NAME must contain, e.g. "LTS"
	TagExclude  string // regex a release TAG must not match, e.g. "-ea"
	FetchCount  int    // releases to scan when filtering (default 30)
}

// releaseQueryFor builds the release lookup for repo from a strategy override.
//
// Both callers MUST go through this. Detection (fillGithubRelease) and resolution
// (upstreamComposeResolver.plan) have to agree on the tag, and they previously
// built the query independently — the resolver dropped NameFilter/TagExclude, so
// a filtered repo could resolve a tag detection had deliberately excluded.
//
// repo is a parameter because detection may use an auto-detected repo rather than
// the override's own GithubRepo.
func releaseQueryFor(repo string, o strategyOverride) releaseQuery {
	return releaseQuery{
		Repo:        repo,
		ReleaseType: orDefault(o.ReleaseType, "latest"),
		NameFilter:  o.NameFilter,
		TagExclude:  o.TagExclude,
	}
}

// latestRelease returns the newest release tag for q. When release_type=latest
// and no name_filter, it uses the cheap /releases/latest endpoint; otherwise it
// scans /releases, drops draft/prerelease (stable) or non-matching names, applies
// the tag-exclude regex, and semver-sorts.
func (g *githubClient) latestRelease(ctx context.Context, q releaseQuery) (string, error) {
	if q.FetchCount <= 0 {
		q.FetchCount = 30
	}
	if q.ReleaseType == "latest" && q.NameFilter == "" {
		var rel ghRelease
		if _, err := g.apiGet(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", githubAPIBase, q.Repo), &rel); err != nil {
			return "", err
		}
		return rel.TagName, nil
	}

	var rels []ghRelease
	if _, err := g.apiGet(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=%d", githubAPIBase, q.Repo, q.FetchCount), &rels); err != nil {
		return "", err
	}

	var excludeRe *regexp.Regexp
	if q.TagExclude != "" {
		excludeRe = regexp.MustCompile(q.TagExclude)
	}
	filtered := rels[:0]
	for _, r := range rels {
		if excludeRe != nil && excludeRe.MatchString(r.TagName) {
			continue
		}
		if r.Draft {
			continue
		}
		if q.NameFilter != "" {
			if !strings.Contains(strings.ToLower(r.Name), strings.ToLower(q.NameFilter)) {
				continue
			}
		} else if q.ReleaseType == "stable" && r.Prerelease {
			continue
		}
		filtered = append(filtered, r)
	}
	if len(filtered) == 0 {
		return "", fmt.Errorf("no matching releases for %s", q.Repo)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return parseSemver(filtered[i].TagName).cmp(parseSemver(filtered[j].TagName)) > 0
	})
	return filtered[0].TagName, nil
}

// ---------------------------------------------------------------------------
// github-branch source — "what is the newest CI build of this GitHub Packages
// image that belongs to the branch I track?".
// ---------------------------------------------------------------------------

// customBranchRe matches a custom/dev tag like "0.15-abc1234-amd64".
var customBranchRe = regexp.MustCompile(`^(\d+\.\d+)(?:\.\d+)?-`)

// shaTailRe extracts the trailing commit SHA from a stripped tag.
var shaTailRe = regexp.MustCompile(`-([a-f0-9]{7,})$`)

// Branch-commit lookback windows. master (exclude) is short — under-covering only
// risks *failing to exclude* an old master build, which is rare and harmless next to
// fresh dev builds. dev (include, when configured) is generous so an active dev
// branch's newest build is always inside the window.
const (
	masterWindowDays = 14
	devWindowDays    = 30
)

type branchQuery struct {
	Owner        string   // ghcr owner (blakeblackshear)
	Package      string   // container package (frigate)
	Repo         string   // owner/repo for commit/branch lookups
	FilterBranch string   // branch to EXCLUDE; default "master"
	DevBranch    string   // branch a build MUST be on; "" ⇒ exclude-only (no membership requirement)
	ArchSuffix   string   // default "amd64"
	TagExcludes  []string // default ["cache","h8l"]
	Registry     string   // default "ghcr.io"
	CurrentTag   string   // the running image tag
}

type branchResult struct {
	VersionStatus string // outdated|updated|unknown
	LatestImage   string // full registry/owner/package:tag-arch
	LatestTag     string
	CurrentParsed string
}

type parsedTag struct {
	custom  bool
	branch  string
	version string
	strip   string
}

func parseCurrentTag(tag, arch string) parsedTag {
	strip := tag
	if arch != "" {
		strip = strings.TrimSuffix(strip, "-"+arch)
	}
	if m := customBranchRe.FindStringSubmatch(strip); m != nil {
		version := strip
		if sm := shaTailRe.FindStringSubmatch(strip); sm != nil {
			version = sm[1]
		}
		return parsedTag{custom: true, branch: m[1], version: version, strip: strip}
	}
	return parsedTag{custom: false, version: strip, strip: strip}
}

// latestPackageVersion runs the frigate-style github-branch check.
func (g *githubClient) latestPackageVersion(ctx context.Context, q branchQuery) (branchResult, error) {
	if q.FilterBranch == "" {
		q.FilterBranch = "master"
	}
	if q.ArchSuffix == "" {
		q.ArchSuffix = "amd64"
	}
	if q.Registry == "" {
		q.Registry = "ghcr.io"
	}
	if len(q.TagExcludes) == 0 {
		q.TagExcludes = []string{"cache", "h8l"}
	}
	cur := parseCurrentTag(q.CurrentTag, q.ArchSuffix)
	res := branchResult{VersionStatus: "unknown", CurrentParsed: cur.strip}

	if cur.custom {
		head, err := g.branchHead(ctx, q.Repo, cur.branch)
		if err != nil || head == "" {
			return res, err
		}
		if g.verifyImage(ctx, q, head) {
			res.LatestTag, res.LatestImage = head, g.imageRefFor(q, head)
			if head != cur.version {
				res.VersionStatus = "outdated"
			} else {
				res.VersionStatus = "updated"
			}
		}
		return res, nil
	}

	// Fetch the exclude set (filter branch, e.g. master), the include set (dev branch,
	// when configured), and the package tags CONCURRENTLY. Fail closed: if ANY fetch
	// errors we return unknown + error rather than risk selecting a master/feature
	// build from incomplete data ("we must filter out master").
	var (
		masterSHAs, devSHAs map[string]bool
		tags                []string
	)
	grp, gctx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		s, e := g.branchCommitSHAs(gctx, q.Repo, q.FilterBranch, masterWindowDays)
		masterSHAs = s
		return e
	})
	requireDev := q.DevBranch != ""
	if requireDev {
		grp.Go(func() error {
			s, e := g.branchCommitSHAs(gctx, q.Repo, q.DevBranch, devWindowDays)
			devSHAs = s
			return e
		})
	}
	grp.Go(func() error {
		t, e := g.packageTags(gctx, q.Owner, q.Package, 5)
		tags = t
		return e
	})
	if err := grp.Wait(); err != nil {
		return res, err
	}
	// A candidate qualifies iff it is NOT a filter-branch (master) build and — when a
	// dev branch is configured — IS a dev-branch build (so feature/dependabot/CI builds
	// on other branches are rejected too).
	candidates := filterDedupeTags(tags, masterSHAs, devSHAs, requireDev, q.TagExcludes)
	if len(candidates) == 0 {
		return res, nil
	}
	for i, tag := range candidates {
		if i >= 5 {
			break
		}
		if g.verifyImage(ctx, q, tag) {
			res.LatestTag, res.LatestImage = tag, g.imageRefFor(q, tag)
			if tag != cur.version {
				res.VersionStatus = "outdated"
			} else {
				res.VersionStatus = "updated"
			}
			return res, nil
		}
	}
	return res, nil
}

func (g *githubClient) imageRefFor(q branchQuery, tag string) string {
	return fmt.Sprintf("%s/%s/%s:%s-%s", q.Registry, q.Owner, q.Package, tag, q.ArchSuffix)
}

func (g *githubClient) verifyImage(ctx context.Context, q branchQuery, tag string) bool {
	ref := parseImageReference(g.imageRefFor(q, tag), "latest")
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return g.registry.imageExists(vctx, ref)
}

// resolveRef returns the full commit SHA a ref currently points at.
//
// Uses /repos/{repo}/commits/{ref}, which resolves a branch, a tag OR a raw SHA
// — unlike branchHead below, whose /branches/{branch} endpoint 404s on a tag.
// One call answers "what would I get if I built this ref right now?" without the
// caller having to know which kind of ref it holds.
//
// Returns the full SHA rather than 7 chars: the caller compares against an
// image's org.opencontainers.image.revision label, which is the full 40.
func (g *githubClient) resolveRef(ctx context.Context, repo, ref string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	// g.apiBase, not the package const: it is what makes the endpoint testable, and
	// pinning WHICH endpoint matters — /branches/{ref} 404s on a tag, so a silent
	// swap would turn every tag-built image into "unknown" with nothing failing.
	if _, err := g.apiGet(ctx, fmt.Sprintf("%s/repos/%s/commits/%s", g.apiBase, repo, ref), &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// branchHead returns the latest commit SHA (7 chars) for a custom branch.
func (g *githubClient) branchHead(ctx context.Context, repo, branch string) (string, error) {
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if _, err := g.apiGet(ctx, fmt.Sprintf("%s/repos/%s/branches/%s", githubAPIBase, repo, branch), &out); err != nil {
		return "", err
	}
	if len(out.Commit.SHA) >= 7 {
		return out.Commit.SHA[:7], nil
	}
	return "", nil
}

// branchCommitSHAs returns recent commit SHAs (7 chars) on filterBranch for the
// last `days`, used to drop CI/continuous builds from the package tag list.
func (g *githubClient) branchCommitSHAs(ctx context.Context, repo, branch string, days int) (map[string]bool, error) {
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02T15:04:05Z")
	url := fmt.Sprintf("%s/repos/%s/commits?sha=%s&since=%s&per_page=100", githubAPIBase, repo, branch, since)
	shas := map[string]bool{}
	for url != "" {
		var commits []struct {
			SHA string `json:"sha"`
		}
		resp, err := g.apiGet(ctx, url, &commits)
		if err != nil {
			// Fail closed: an incomplete branch-commit set would silently disable the
			// master-exclude / dev-include filter and could select a master build.
			return nil, fmt.Errorf("list %s commits for %s: %w", branch, repo, err)
		}
		for _, c := range commits {
			if len(c.SHA) >= 7 {
				shas[c.SHA[:7]] = true
			}
		}
		url = nextLink(resp.Header.Get("Link"))
	}
	return shas, nil
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(link string) string {
	if m := nextLinkRe.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}

// packageTags fetches all container tags from the GH Packages API (paginated).
func (g *githubClient) packageTags(ctx context.Context, owner, pkg string, maxPages int) ([]string, error) {
	var all []string
	for page := 1; page <= maxPages; page++ {
		url := fmt.Sprintf("%s/users/%s/packages/container/%s/versions?per_page=100&page=%d", githubAPIBase, owner, pkg, page)
		var versions []struct {
			Metadata struct {
				Container struct {
					Tags []string `json:"tags"`
				} `json:"container"`
			} `json:"metadata"`
		}
		if _, err := g.apiGet(ctx, url, &versions); err != nil {
			if page == 1 {
				return nil, err
			}
			break
		}
		if len(versions) == 0 {
			break
		}
		for _, v := range versions {
			all = append(all, v.Metadata.Container.Tags...)
		}
		if len(versions) < 100 {
			break
		}
	}
	return all, nil
}

// filterDedupeTags drops excluded tags, strips arch variants (everything after the
// first '-'), dedupes preserving order, removes filter-branch (master) builds, and —
// when requireDev is set — keeps ONLY builds whose SHA is on the dev branch (rejecting
// release/feature/dependabot builds that are neither master nor dev).
func filterDedupeTags(tags []string, masterSHAs, devSHAs map[string]bool, requireDev bool, excludes []string) []string {
	var excludeRe *regexp.Regexp
	if len(excludes) > 0 {
		excludeRe = regexp.MustCompile("(?i)" + strings.Join(excludes, "|"))
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if excludeRe != nil && excludeRe.MatchString(t) {
			continue
		}
		stripped := t
		if i := strings.Index(stripped, "-"); i >= 0 {
			stripped = stripped[:i]
		}
		if stripped == "" || seen[stripped] || masterSHAs[stripped] {
			continue
		}
		if requireDev && !devSHAs[stripped] {
			continue // not a dev-branch build → reject (dev-only policy)
		}
		seen[stripped] = true
		out = append(out, stripped)
	}
	return out
}
