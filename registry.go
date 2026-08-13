package main

// Container-registry digest client.
//
//   - parseImageReference: split registry/repository:tag(@digest)
//   - getRegistryDigest: HEAD /v2/<repo>/manifests/<tag> with OCI Accept headers,
//     bearer re-auth on 401, Docker Hub library/ prefix for single-segment repos.
//
// One client, two uses: the registry-digest VersionSource ("has the digest behind
// this moving tag changed?") and the registry-existence gate ("is the tag this
// resolver wants actually pushed?"). Private-registry auth comes from the mounted
// ~/.docker/config.json (RegistryAuthFile); anonymous bearer flow otherwise
// (Docker Hub, public ghcr, an unauthenticated self-hosted v2 registry).

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// manifestAccept advertises every manifest media type a registry might answer
// with so the HEAD returns a Docker-Content-Digest for multi-arch indexes too.
const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json"

var wwwAuthRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

// imageRef is a parsed container image reference.
type imageRef struct {
	Registry   string // "" means Docker Hub (registry-1.docker.io)
	Repository string
	Tag        string
	Digest     string // sha256:... when pinned by digest, else ""
	FullRepo   string // registry/repository (no tag/digest)
}

// String renders the canonical name:tag (or name@digest) form.
func (r imageRef) String() string {
	if r.Digest != "" {
		return r.FullRepo + "@" + r.Digest
	}
	return r.FullRepo + ":" + r.Tag
}

// parseImageReference splits an image string into components. Handles
// registry[:port]/repo[:tag][@sha256:...] and bare repo:tag forms (a host is
// recognised only when the first path segment contains '.' or ':').
func parseImageReference(image, defaultTag string) imageRef {
	if defaultTag == "" {
		defaultTag = "latest"
	}
	var digest string
	if i := strings.Index(image, "@sha256:"); i >= 0 {
		digest = image[i+1:]
		image = image[:i]
	}

	repo, tag := image, defaultTag
	// A tag exists only if the last path segment contains a colon.
	if last := image[strings.LastIndex(image, "/")+1:]; strings.Contains(last, ":") {
		if i := strings.LastIndex(image, ":"); i >= 0 {
			repo, tag = image[:i], image[i+1:]
		}
	}

	registry, repository := "", repo
	if i := strings.Index(repo, "/"); i >= 0 {
		first := repo[:i]
		if strings.ContainsAny(first, ".:") {
			registry, repository = first, repo[i+1:]
		}
	}
	return imageRef{Registry: registry, Repository: repository, Tag: tag, Digest: digest, FullRepo: repo}
}

// dockerAuthConfig is the subset of ~/.docker/config.json we read.
type dockerAuthConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

type cachedToken struct {
	token string
	exp   time.Time
}

// registryClient performs digest/existence queries against OCI v2 registries with
// a shared pooled HTTP client, optional creds from the docker config, and a small
// per-scope bearer-token cache.
type registryClient struct {
	hc       *http.Client
	auths    map[string]string // normalised registry host -> "user:pass"
	insecure map[string]bool   // hosts to skip TLS verify for (self-signed)

	mu     sync.Mutex
	tokens map[string]cachedToken // realm+scope -> token
}

func newRegistryClient(authFile string, insecureHosts []string) *registryClient {
	insecure := map[string]bool{}
	for _, h := range insecureHosts {
		if h = strings.TrimSpace(h); h != "" {
			insecure[h] = true
		}
	}
	// One transport; if any insecure hosts are configured we still verify the
	// rest (per-request we can't swap TLS config, so a single insecure host opts
	// the client into skip-verify — kept narrow via the insecure set check in
	// dialing is not possible here, so we only skip when the set is non-empty AND
	// the registry is listed). Simplest correct approach: a verifying transport,
	// plus a second skip-verify transport selected per request.
	rc := &registryClient{
		hc:       &http.Client{Timeout: 20 * time.Second, Transport: pooledTransport(false)},
		auths:    loadDockerAuths(authFile),
		insecure: insecure,
		tokens:   map[string]cachedToken{},
	}
	return rc
}

func pooledTransport(skipVerify bool) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	if skipVerify {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 — explicit per-host opt-in
	}
	return t
}

// loadDockerAuths reads ~/.docker/config.json (RegistryAuthFile) and returns a
// map of registry host -> "user:pass". Missing/unreadable file = no creds.
func loadDockerAuths(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var cfg dockerAuthConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return out
	}
	for host, a := range cfg.Auths {
		userpass := ""
		if a.Auth != "" {
			if dec, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				userpass = string(dec)
			}
		} else if a.Username != "" {
			userpass = a.Username + ":" + a.Password
		}
		if userpass != "" {
			out[normalizeAuthHost(host)] = userpass
		}
	}
	return out
}

// normalizeAuthHost strips the scheme/path docker writes for the Hub key
// ("https://index.docker.io/v1/") down to a bare host.
func normalizeAuthHost(host string) string {
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	return host
}

// lookupAuth returns "user:pass" creds for a registry host, accounting for the
// several aliases Docker Hub is stored under.
func (rc *registryClient) lookupAuth(registry string) string {
	if registry == "registry-1.docker.io" || registry == "docker.io" || registry == "" {
		for _, k := range []string{"registry-1.docker.io", "index.docker.io", "docker.io", "auth.docker.io"} {
			if v := rc.auths[k]; v != "" {
				return v
			}
		}
		return ""
	}
	return rc.auths[registry]
}

func (rc *registryClient) clientFor(registry string) *http.Client {
	if rc.insecure[registry] {
		return &http.Client{Timeout: 20 * time.Second, Transport: pooledTransport(true)}
	}
	return rc.hc
}

// normalizeForHub maps the empty/docker.io registry to registry-1.docker.io and
// prefixes single-segment repositories with library/ (official images).
func normalizeForHub(ref imageRef) (registry, repository string) {
	registry, repository = ref.Registry, ref.Repository
	if registry == "" {
		registry = "registry-1.docker.io"
	}
	if registry == "registry-1.docker.io" || registry == "docker.io" {
		registry = "registry-1.docker.io"
		if !strings.Contains(repository, "/") {
			repository = "library/" + repository
		}
	}
	return registry, repository
}

// getRegistryDigest returns the manifest digest for ref's tag, performing the
// anonymous (or cred-backed) bearer dance on 401. Empty registries resolve to
// Docker Hub. Used both for digest comparison and (via imageExists) the
// existence gate.
func (rc *registryClient) getRegistryDigest(ctx context.Context, ref imageRef) (string, error) {
	registry, repository := normalizeForHub(ref)
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repository, ref.Tag)
	hc := rc.clientFor(registry)

	digest, status, wwwAuth, err := rc.manifestHead(ctx, hc, url, "")
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		return digest, nil
	}
	if status == http.StatusUnauthorized && strings.Contains(wwwAuth, "Bearer") {
		token, terr := rc.bearerToken(ctx, hc, wwwAuth, registry, repository)
		if terr != nil {
			return "", fmt.Errorf("registry auth %s/%s: %w", registry, repository, terr)
		}
		if token != "" {
			digest, status, _, err = rc.manifestHead(ctx, hc, url, "Bearer "+token)
			if err != nil {
				return "", err
			}
			if status == http.StatusOK {
				return digest, nil
			}
		}
		return "", fmt.Errorf("registry %s/%s: HTTP %d after token auth", registry, repository, status)
	}
	return "", fmt.Errorf("registry %s/%s: HTTP %d", registry, repository, status)
}

// manifestHead issues a HEAD (falling back to GET on 405) and returns the
// Docker-Content-Digest, the status, and any WWW-Authenticate header.
func (rc *registryClient) manifestHead(ctx context.Context, hc *http.Client, url, authz string) (digest string, status int, wwwAuth string, err error) {
	digest, status, wwwAuth, err = rc.manifestReq(ctx, hc, http.MethodHead, url, authz)
	if err == nil && status == http.StatusMethodNotAllowed {
		// Some registries reject HEAD on manifests — GET and hash the body if no
		// digest header is present.
		return rc.manifestReq(ctx, hc, http.MethodGet, url, authz)
	}
	return digest, status, wwwAuth, err
}

func (rc *registryClient) manifestReq(ctx context.Context, hc *http.Client, method, url, authz string) (string, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return "", 0, "", err
	}
	req.Header.Set("Accept", manifestAccept)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
			return d, resp.StatusCode, "", nil
		}
		if method == http.MethodGet {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			sum := sha256.Sum256(body)
			return fmt.Sprintf("sha256:%x", sum), resp.StatusCode, "", nil
		}
	}
	return "", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), nil
}

// bearerToken obtains a registry token from the realm in the WWW-Authenticate
// header (cached per realm+scope), attaching Basic creds when we have them.
func (rc *registryClient) bearerToken(ctx context.Context, hc *http.Client, wwwAuth, registry, repository string) (string, error) {
	params := map[string]string{}
	for _, m := range wwwAuthRe.FindAllStringSubmatch(wwwAuth, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("no realm in WWW-Authenticate")
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repository + ":pull"
	}
	cacheKey := realm + "|" + params["service"] + "|" + scope

	rc.mu.Lock()
	if ct, ok := rc.tokens[cacheKey]; ok && time.Now().Before(ct.exp) {
		rc.mu.Unlock()
		return ct.token, nil
	}
	rc.mu.Unlock()

	tokenURL := realm + "?scope=" + scope
	if svc := params["service"]; svc != "" {
		tokenURL += "&service=" + svc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	if creds := rc.lookupAuth(registry); creds != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("token endpoint %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", err
	}
	token := tr.Token
	if token == "" {
		token = tr.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("empty token from realm")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	rc.mu.Lock()
	rc.tokens[cacheKey] = cachedToken{token: token, exp: time.Now().Add(ttl - 10*time.Second)}
	rc.mu.Unlock()
	return token, nil
}

// imageExists reports whether ref resolves to a manifest (the existence gate for
// repo-driven version candidates — never propose a tag whose image isn't pushed).
func (rc *registryClient) imageExists(ctx context.Context, ref imageRef) bool {
	_, err := rc.getRegistryDigest(ctx, ref)
	return err == nil
}
