package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every distinct image reference running in the estate at the time this was
// written. The over-strictness guard: a validator that rejects a legitimate
// current value is worse than the hole it closes, because it breaks the working
// path to protect against one nobody has exercised.
//
// Includes the shapes a hand-written grammar tends to miss — a registry port, a
// multi-segment ghcr path, an `@sha256:` digest, and two containers reporting a
// BARE digest as their image.
var realFleetImages = []string{
	"cloudflare/cloudflared:latest",
	"container-registry-api.kdhomeapps.com:1443/build-agent:latest",
	"container-registry-api.kdhomeapps.com:1443/docker-agent:latest",
	"container-registry-api.kdhomeapps.com:1443/duplicacy-agent-api:20260814-020401",
	"container-registry-api.kdhomeapps.com:1443/filemesh-agent:20260721-141947",
	"container-registry-api.kdhomeapps.com:1443/frigate:20260901-001836",
	"container-registry-api.kdhomeapps.com:1443/gdrive-agent:latest",
	"container-registry-api.kdhomeapps.com:1443/genmon:2.0.01",
	"container-registry-api.kdhomeapps01.com:1443/docker-agent:latest",
	"container-registry-api.nghomeapps.com:1443/build-agent:latest",
	"container-registry-api.nghomeapps.com:1443/docker-agent:latest",
	"container-registry-api.nghomeapps.com:1443/duplicacy-agent-api:20260814-020401",
	"container-registry-api.nghomeapps.com:1443/gdrive-agent:latest",
	"container-registry-api.nghomeapps.com:1443/genmon:2.0.01",
	"cturra/ntp:latest",
	"docker.io/valkey/valkey:9@sha256:8e8d64b405ce18f41b8e5ee20aa4687a8ed0022d1298f2ce31cdcf3a76e09411",
	"ghcr.io/blakeblackshear/frigate:ca18b8d-amd64",
	"ghcr.io/home-assistant/home-assistant:stable",
	"ghcr.io/immich-app/immich-machine-learning:release",
	"ghcr.io/immich-app/immich-server:release",
	"ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0@sha256:bcf63357191b76a916ae5eb93464d65c07511da41e3bf7a8416db519b40b1c23",
	"ghcr.io/matter-js/matterjs-server:1.4.0",
	"ghcr.io/ownbee/hass-otbr-docker:v0.3.0",
	"ghcr.io/tailscale/tailscale:latest",
	"instantlinux/nut-upsd:latest",
	"moby/buildkit:buildx-stable-1",
	"nginx:alpine",
	"postgres:16-alpine",
	"sha256:15dc409d48a2a475ef6e54b26e211a13a97f5ea5d0b7f20bf2ee22c37ea33237",
	"sha256:65688cd5e2071f581c8cdca102574db86b64541bb0b18f87d04b4ec3ba6096ec",
	"traefik:v3.7.12",
	"vaultwarden/server:latest",
	"zwavejs/zwave-js-ui:latest",
}

func TestValidImageRefAcceptsEveryImageInTheEstate(t *testing.T) {
	if len(realFleetImages) < 30 {
		t.Fatalf("corpus shrank to %d — it is the whole point of this test", len(realFleetImages))
	}
	for _, img := range realFleetImages {
		if !validImageRef(img) {
			t.Errorf("rejected a real image: %q", img)
		}
	}
}

func TestValidImageRefRefusesWhatWouldCorruptAFile(t *testing.T) {
	const base = "registry.example.invalid:1443/app:latest"
	cases := []struct{ in, why string }{
		{"", "empty"},
		{"reg/app:latest\nEXTRA=pwned", "🔴 the newline that appends a line to .env"},
		// Every rune here is otherwise ALLOWED, so this fails for the newline and
		// nothing else. The case above contains '=' and would be rejected on the
		// charset even if newlines were permitted — it passed for the wrong reason.
		{"reg/app:latest\nreg/other:latest", "newline alone, no other illegal rune"},
		{"reg/app:latest\rEXTRA=pwned", "carriage return"},
		{"reg/app:latest\tx", "tab"},
		{"reg/app latest", "internal space"},
		{" " + base, "leading space"},
		{base + " ", "trailing space"},
		{"reg/app:latest\x00", "NUL"},
		{"reg/${OTHER}:latest", "compose interpolates $ in a .env value"},
		{"reg/app:latest$VAR", "bare $"},
		{"reg/app:latest;rm -rf /", "shell metacharacter"},
		{"reg/app:latest|tee", "pipe"},
		{"reg/app:latest&", "ampersand"},
		{"reg/app:`id`", "backtick"},
		{`reg/app:"quoted"`, "double quote"},
		{"reg/app:'quoted'", "single quote"},
		{`reg\app:latest`, "backslash"},
		{"/reg/app:latest", "must start with a host or repository component"},
		{"-reg/app:latest", "leading dash could read as a flag"},
		{"reg//app:latest", "empty path segment"},
		{"reg/app@sha256:aa@sha256:bb", "two digests"},
		{strings.Repeat("a", maxImageRefLen+1), "over the length cap"},
	}
	for _, c := range cases {
		if validImageRef(c.in) {
			t.Errorf("%s: accepted %q", c.why, c.in)
		}
	}
	// Positive control: the base the negatives are built from must be accepted, so
	// a validator that rejected everything could not pass this test.
	if !validImageRef(base) {
		t.Fatalf("positive control rejected: %q", base)
	}
	if !validImageRef(strings.Repeat("a", maxImageRefLen)) {
		t.Error("exactly at the cap must pass")
	}
}

// The boundary check cannot be driven without a full *app, so it is pinned at the
// source — scoped to this one function's body, and asserting ORDER: validating
// after the job has started would be no validation at all.
func TestHandlerValidatesBeforeStartingTheJob(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "compose_handlers.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleComposeOp" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("handleComposeOp not found — this guard is scoped to it and cannot run")
	}
	validatePos, startPos := -1, -1
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "validImageRef" && validatePos < 0 {
				validatePos = int(call.Pos())
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "start" && startPos < 0 {
				startPos = int(call.Pos())
			}
		}
		return true
	})
	if validatePos < 0 {
		t.Fatal("handleComposeOp must validate override_image before dispatching it")
	}
	if startPos < 0 {
		t.Fatal("no reg.start call found — the guard has lost its subject")
	}
	if validatePos > startPos {
		t.Error("validImageRef runs AFTER the job starts; the value is already in flight")
	}
}

func TestSafeEnvLineValue(t *testing.T) {
	for _, ok := range []string{"reg/app:latest", "plain", "a b", "with=equals", "$dollar"} {
		if !safeEnvLineValue(ok) {
			t.Errorf("this guard is only about LINE integrity; %q should pass", ok)
		}
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\x00b", "a\x1bb", "a\x7fb"} {
		if safeEnvLineValue(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// setEnvVar must REFUSE, and must not have touched the file. A guard that rejects
// after a partial write is not a guard.
func TestSetEnvVarRefusesAMultilineValueAndLeavesTheFileIntact(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	const original = "IMAGE=reg/app:v1\nOTHER=keepme\n"
	if err := os.WriteFile(envPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: ${IMAGE}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := resolveLoadPaths(ProjectEntry{Name: "p", WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := setEnvVar(paths, dir, "IMAGE", "reg/app:v2\nINJECTED=yes"); err == nil {
		t.Error("a value with a newline must be refused")
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("file was modified by a refused write:\n%q", got)
	}
	if strings.Contains(string(got), "INJECTED") {
		t.Error("🔴 the injected line reached the .env")
	}

	// Positive control: a legitimate value still writes, so the refusal above is
	// the guard working rather than setEnvVar being broken.
	if _, err := setEnvVar(paths, dir, "IMAGE", "reg/app:v2"); err != nil {
		t.Fatalf("a valid value must still be written: %v", err)
	}
	after, _ := os.ReadFile(envPath)
	if !strings.Contains(string(after), "IMAGE=reg/app:v2") || !strings.Contains(string(after), "OTHER=keepme") {
		t.Errorf("valid write did not land or clobbered a sibling:\n%q", after)
	}
}
