package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestComposeImagesLiveContentsAPI exercises the real production path — the raw
// URL rewrite, the authenticated Contents API, and the parse — against GitHub
// itself. It is the only check that proves an *installation* token is authorized
// to read the contents of a public repo the App is not installed on, which is the
// assumption the whole upstream-compose fix rests on.
//
// Skipped unless a token is supplied, so CI and the normal `go test ./...` stay
// hermetic:
//
//	TOK=$(curl -s -X POST "http://$ROUTER_IP:5000/api/docker/github-token" \
//	  -H "Authorization: Bearer $(sudo cat /srv/containers/docker-agent/bearer-token)" \
//	  -H 'content-type: application/json' -d '{"repositories":[]}' | jq -r .token)
//	DOCKER_AGENT_GH_TOKEN=$TOK go test -run TestComposeImagesLive -v .
func TestComposeImagesLiveContentsAPI(t *testing.T) {
	token := os.Getenv("DOCKER_AGENT_GH_TOKEN")
	if token == "" {
		t.Skip("set DOCKER_AGENT_GH_TOKEN to a GitHub App installation token to run the live check")
	}

	g := newGithubClient(nil, "", nil)
	g.token, g.tokenExp = token, time.Now().Add(time.Hour)

	const tag = "v3.0.1"
	url := "https://" + rawGithubHost + "/immich-app/immich/" + tag + "/docker/docker-compose.yml"

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	images, err := g.composeImages(ctx, url, map[string]string{"IMMICH_VERSION": tag})
	if err != nil {
		t.Fatalf("composeImages against the live Contents API: %v", err)
	}

	// The two driver services must land on the concrete release tag, not the
	// floating ${IMMICH_VERSION:-release} default.
	want := map[string]string{
		"immich-server":           "ghcr.io/immich-app/immich-server:" + tag,
		"immich-machine-learning": "ghcr.io/immich-app/immich-machine-learning:" + tag,
	}
	for svc, img := range want {
		if images[svc] != img {
			t.Errorf("%s = %q, want %q", svc, images[svc], img)
		}
	}
	// The coupled services are digest-pinned upstream and must survive verbatim.
	for _, svc := range []string{"redis", "database"} {
		if images[svc] == "" {
			t.Errorf("%s missing from the upstream compose", svc)
		}
	}

	// A second resolve must be served from cache — no second network call.
	if _, ok := g.cachedCompose(url); !ok {
		t.Fatal("the fetched compose was not cached")
	}
	t.Logf("live Contents API OK: %d services, immich-server=%s", len(images), images["immich-server"])
}
