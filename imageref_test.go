package main

import (
	"github.com/distribution/reference"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"slices"
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
	"traefik:v3.7.12",
	"vaultwarden/server:latest",
	"zwavejs/zwave-js-ui:latest",
}

// The two esphome containers run from bare image IDs: the legacy Portainer rollback
// wrote the ID into CURRENT_ESPHOME_IMAGE and the migration copied it into .env.
// An ID is refused as an image to deploy — it is the defect, not a value to keep.
var realFleetImageIDs = []string{
	"sha256:15dc409d48a2a475ef6e54b26e211a13a97f5ea5d0b7f20bf2ee22c37ea33237",
	"sha256:65688cd5e2071f581c8cdca102574db86b64541bb0b18f87d04b4ec3ba6096ec",
	"15dc409d48a2a475ef6e54b26e211a13a97f5ea5d0b7f20bf2ee22c37ea33237",
}

func TestValidImageRefRefusesImageIDs(t *testing.T) {
	for _, id := range realFleetImageIDs {
		if validImageRef(id) || !isImageID(id) {
			t.Errorf("%q: valid=%v isImageID=%v", id, validImageRef(id), isImageID(id))
		}
	}
	for _, img := range realFleetImages {
		if isImageID(img) {
			t.Errorf("a named reference read as an image ID: %q", img)
		}
	}
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
		{"traefik:", "an empty tag — the grammar, not the charset"},
		{"traefik@", "an empty digest"},
		{"traefik:v3:x", "two tags"},
		{"Traefik:v3", "an uppercase repository"},
		{"reg/app@sha256:abc", "a truncated digest"},
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
	// A valid reference exactly at the cap: a 128-byte tag and a sha512 digest
	// (exactly 128 hex), with the name taking the rest.
	suffix := ":" + strings.Repeat("t", 128) + "@sha512:" + strings.Repeat("0", 128)
	name := "reg.example/" + strings.Repeat("a", maxImageRefLen-len(suffix)-len("reg.example/"))
	atCap := name + suffix
	if len(atCap) != maxImageRefLen || !validImageRef(atCap) {
		t.Errorf("a valid reference exactly at the cap (%d bytes) must pass", len(atCap))
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

// The env writer refuses a value that would add lines, whatever validated it
// upstream: the guard holds for every caller, not only today's one boundary.
func TestPlanVarRefusesAValueThatWouldAddLines(t *testing.T) {
	p := &writePlan{files: map[string]*fileState{}, vars: map[string]*varEdit{}}
	for _, v := range []string{"reg/app:v2\nINJECTED=yes", "reg/app:v2\rX=1", "a b", "a#b", `a"b`, "a$b"} {
		if err := p.planVar("IMAGE", v, "app", "", 0); err == nil {
			t.Errorf("planVar accepted %q", v)
		}
	}
	if len(p.files) != 0 {
		t.Error("a refused value was planned into a file")
	}
}

// validImageRef agrees with docker's grammar on every input, except where one of
// its three documented extra rules says otherwise: an image ID, the length cap,
// or a rune outside the grammar's alphabet (which the grammar never accepts).
func TestValidImageRefAgreesWithTheGrammar(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260917, 1))
	alphabet := []rune("abcxyz09AZ._-/:@+[]$ \"'\\#\n\t%!=")
	hosts := []string{"", "reg.example/", "reg.example:5000/", "localhost:5000/", "[::1]:5000/", "[fe80::1]/", "[::1]/", "10.0.0.1:443/", "[zz]/"}
	names := []string{"a", "a-b", "a__b", "library/nginx", "Up", "x.y_z", "a/b/c", ""}
	tags := []string{"", ":v1", ":V1.2-rc", ":", ":v3:x", ":" + strings.Repeat("t", 129)}
	digests := []string{"", "@sha256:" + strings.Repeat("a", 64), "@sha256:abc", "@", "@sha512:" + strings.Repeat("0", 128)}
	gen := func() string {
		s := hosts[rng.IntN(len(hosts))] + names[rng.IntN(len(names))] + tags[rng.IntN(len(tags))] + digests[rng.IntN(len(digests))]
		for range rng.IntN(3) {
			rs := []rune(s)
			i := rng.IntN(len(rs) + 1)
			switch rng.IntN(3) {
			case 0:
				rs = slices.Insert(rs, i, alphabet[rng.IntN(len(alphabet))])
			case 1:
				if i < len(rs) {
					rs = slices.Delete(rs, i, i+1)
				}
			default:
				if i < len(rs) {
					rs[i] = alphabet[rng.IntN(len(alphabet))]
				}
			}
			s = string(rs)
		}
		return s
	}
	extra := func(s string) bool { return isImageID(s) || len(s) > maxImageRefLen }
	overCap := "reg.example/" + strings.Repeat("a", 243) + ":" + strings.Repeat("t", 128) + "@sha512:" + strings.Repeat("0", 128)
	if _, err := reference.ParseNormalizedNamed(overCap); err != nil || len(overCap) <= maxImageRefLen {
		t.Fatalf("fixture: %d bytes, grammar err %v", len(overCap), err)
	}
	inputs := append([]string{"[::1]:5000/a-b", "[fe80::1%eth0]:5000/a", "sha256:" + strings.Repeat("f", 64), overCap}, realFleetImages...)
	for range 200_000 {
		inputs = append(inputs, gen())
	}
	disagree, accepted, ipv6 := 0, 0, 0
	for _, s := range inputs {
		_, err := reference.ParseNormalizedNamed(s)
		grammar := err == nil
		want := grammar && !extra(s)
		got := validImageRef(s)
		if got {
			accepted++
			if strings.HasPrefix(s, "[") {
				ipv6++
			}
		}
		if got != want {
			if disagree++; disagree <= 10 {
				t.Errorf("%q: validImageRef=%v, grammar=%v, extra rule=%v", s, got, grammar, extra(s))
			}
		}
		if grammar && !extra(s) && strings.IndexFunc(s, func(r rune) bool { return !fileSafeImageRune(r) }) >= 0 {
			t.Errorf("%q: the grammar accepts a rune outside fileSafeImageRune — the file-safety rule would disagree", s)
		}
	}
	if disagree > 0 {
		t.Fatalf("%d of %d inputs disagree with the grammar", disagree, len(inputs))
	}
	if accepted < 1000 || ipv6 == 0 {
		t.Fatalf("the corpus barely reaches the valid space (%d accepted, %d IPv6): it proves little", accepted, ipv6)
	}
}
