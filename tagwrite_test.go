package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// writeFixture is a project under a real compose root, loaded by the agent's own
// loader.
type writeFixture struct {
	l     *loadEnv
	entry ProjectEntry
	dir   string
}

func newWriteFixture(t *testing.T, files map[string]string, entry ProjectEntry) *writeFixture {
	t.Helper()
	l := newLoadEnv(t)
	name := "wp"
	dir := l.project(t, name, files)
	entry.Name, entry.WorkingDir = name, dir
	return &writeFixture{l: l, entry: entry, dir: dir}
}

func (f *writeFixture) write(targets map[string]string) (*imageWrite, error) {
	return writeServiceImages(context.Background(), f.entry, targets, f.l.cb.loadProject)
}

func (f *writeFixture) image(t *testing.T, service string) string {
	t.Helper()
	p, err := f.l.cb.loadProject(context.Background(), f.entry)
	if err != nil {
		t.Fatal(err)
	}
	return p.Services[service].Image
}

// state is every file under the project, with its bytes.
func (f *writeFixture) state(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(f.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.dir, path)
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameState(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s: %s = %q, want %q", what, name, got[name], body)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: %s exists and should not", what, name)
		}
	}
}

const (
	oldImg = "reg.example/app:v1"
	newImg = "reg.example/app:v2"
)

// Every write lands where compose reads the image — the tag deployed and the tag
// the next `compose up` reads agree — changes one file, and reverts byte for byte.
func TestImageWriteFollowsComposeResolution(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		entry   ProjectEntry
		written string // the one file the write may change (or create)
		after   string // its content after the write, when checked exactly
	}{
		{
			name: "default discovery: the override's literal image wins",
			files: map[string]string{
				"compose.yaml":          "services:\n  app:\n    image: reg.example/app:base\n",
				"compose.override.yaml": "services:\n  app:\n    image: " + oldImg + "\n",
			},
			written: "compose.override.yaml",
			after:   "services:\n  app:\n    image: " + newImg + "\n",
		},
		{
			name: "declared compose files: the last one that sets the image",
			files: map[string]string{
				"a.yml": "services:\n  app:\n    image: reg.example/app:base\n",
				"b.yml": "services:\n  app:\n    image: " + oldImg + "\n",
				"c.yml": "services:\n  app:\n    environment:\n      X: y\n",
			},
			entry:   ProjectEntry{ComposeFiles: []string{"a.yml", "b.yml", "c.yml"}},
			written: "b.yml",
		},
		{
			name: "env files: the last one declaring the variable",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMAGE}\n",
				"one.env":      "TW_IMAGE=reg.example/app:base\n",
				"two.env":      "TW_IMAGE=" + oldImg + "\n",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "two.env",
			after:   "TW_IMAGE=" + newImg + "\n",
		},
		{
			name: "env files: a later file that does not declare it is not the one",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMAGE}\n",
				"one.env":      "TW_IMAGE=" + oldImg + "\n",
				"two.env":      "OTHER=x\n",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "one.env",
		},
		{
			name: "S6: a later `export` assignment is the one compose uses, and is rewritten in place",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMAGE}\n",
				".env":         "TW_IMAGE=reg.example/app:base\nexport TW_IMAGE=" + oldImg + "\n",
			},
			written: ".env",
			after:   "TW_IMAGE=reg.example/app:base\nexport TW_IMAGE=" + newImg + "\n",
		},
		{
			name: "S7: a later yaml-style `KEY: value` assignment",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMAGE}\n",
				".env":         "TW_IMAGE=reg.example/app:base\nTW_IMAGE: " + oldImg + "\n",
			},
			written: ".env",
			after:   "TW_IMAGE=reg.example/app:base\nTW_IMAGE: " + newImg + "\n",
		},
		{
			name: "an undeclared variable with explicit env files: appended to the last",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMAGE-" + oldImg + "}\n",
				"one.env":      "OTHER=x\n",
				"two.env":      "LAST=y",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "two.env",
			after:   "LAST=y\nTW_IMAGE=" + newImg + "\n",
		},
		{
			name: "an undeclared variable and no env file: the working dir's .env, created",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMAGE-" + oldImg + "}\n",
			},
			written: ".env",
			after:   "TW_IMAGE=" + newImg + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriteFixture(t, tc.files, tc.entry)
			if got := f.image(t, "app"); got != oldImg {
				t.Fatalf("fixture: resolves %q, want %q", got, oldImg)
			}
			original := f.state(t)

			w, err := f.write(map[string]string{"app": newImg})
			if err != nil {
				t.Fatal(err)
			}
			if got := f.image(t, "app"); got != newImg {
				t.Errorf("after the write compose resolves %q, want %q", got, newImg)
			}
			if got := w.project.Services["app"].Image; got != newImg {
				t.Errorf("the verified project carries %q", got)
			}
			after := f.state(t)
			for name, body := range after {
				switch {
				case name == tc.written:
					if tc.after != "" && body != tc.after {
						t.Errorf("%s = %q, want %q", name, body, tc.after)
					}
				case body != original[name]:
					t.Errorf("%s changed, but compose takes the image from %s", name, tc.written)
				}
			}

			if err := w.revert(context.Background(), nil, f.l.cb.loadProject); err != nil {
				t.Fatal(err)
			}
			sameState(t, "after the revert", f.state(t), original)
			if got := f.image(t, "app"); got != oldImg {
				t.Errorf("after the revert compose resolves %q, want %q", got, oldImg)
			}
		})
	}
}

// Every refused write leaves every file exactly as it was.
func TestImageWriteRefusesWithNothingChanged(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		entry   ProjectEntry
		targets map[string]string
		env     map[string]string // process environment for the case
		reason  string            // a substring of the refusal
	}{
		{"H1: a trailing ':' is not an image reference",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": "reg.example/app:"}, nil, "not an image reference"},
		{"D: a trailing '@'",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": "traefik@"}, nil, "not an image reference"},
		{"D: two tags",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": "traefik:v3:x"}, nil, "not an image reference"},
		{"G: an image ID",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": "sha256:15dc409d48a2a475ef6e54b26e211a13a97f5ea5d0b7f20bf2ee22c37ea33237"}, nil, "local image ID"},
		{"H5 / S5: an anchored image other services alias",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: &img " + oldImg + "\n  worker:\n    image: *img\n"}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "the image is anchored (&img)"},
		{"H5: an aliased image",
			map[string]string{"compose.yaml": "x-img: &img " + oldImg + "\nservices:\n  app:\n    image: *img\n"}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "the image is a YAML alias (*img)"},
		{"a merge key",
			map[string]string{"compose.yaml": "x-base: &base\n  image: " + oldImg + "\nservices:\n  app:\n    <<: *base\n"}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "takes its keys from a YAML merge key"},
		{"a tagged scalar",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: !!str " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "tagged, block or flow"},
		{"a block scalar",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: >-\n      " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "tagged, block or flow"},
		{"an escaped double-quoted scalar",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: \"reg.example/app:\\x761\"\n"}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "escapes"},
		{"S12: a numeric image would not read back as a string",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: " + oldImg + "\n"}, ProjectEntry{},
			map[string]string{"app": "1234"}, nil, "would not read back"},
		{"S2: a variable registry with a literal tag",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_REGISTRY:-docker.io}/library/traefik:v3.1\n"}, ProjectEntry{},
			map[string]string{"app": "docker.io/library/traefik:v3.2"}, nil, "will not guess"},
		{"a trailing tag variable asked for a different repository",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: reg.example/app:${TW_TAG:-v1}\n"}, ProjectEntry{},
			map[string]string{"app": "reg.example/other:v2"}, nil, "with a different tag"},
		{"S13: the agent's own environment shadows every env file",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_SHADOW}\n", ".env": "TW_SHADOW=" + newImg + "\n"}, ProjectEntry{},
			map[string]string{"app": "reg.example/app:v3"}, map[string]string{"TW_SHADOW": newImg}, "agent's own environment"},
		{"S15: another service shares the variable",
			map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_SHARED}\n  worker:\n    image: ${TW_SHARED}\n",
				".env":         "TW_SHARED=" + oldImg + "\n",
			}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "worker's image changed"},
		{"another service's label interpolates the variable",
			map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n  web:\n    image: nginx:alpine\n    labels:\n      pinned: ${TW_IMG}\n",
				".env":         "TW_IMG=" + oldImg + "\n",
			}, ProjectEntry{},
			map[string]string{"app": newImg}, nil, "web's labels changed"},
		{"two targets set one variable to different images",
			map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_SHARED}\n  worker:\n    image: ${TW_SHARED}\n",
				".env":         "TW_SHARED=" + oldImg + "\n",
			}, ProjectEntry{},
			map[string]string{"app": newImg, "worker": "reg.example/app:v3"}, nil, "share the variable"},
		{"a bare line inheriting the variable is the declaration compose uses",
			map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TW_INH}\n",
				"one.env":      "TW_INH=" + oldImg + "\n",
				"two.env":      "TW_INH\n",
			}, ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			map[string]string{"app": newImg}, nil, "bare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			f := newWriteFixture(t, tc.files, tc.entry)
			original := f.state(t)
			_, err := f.write(tc.targets)
			// The fixture's directory is named after the test case, so the reason is
			// matched with paths removed — otherwise a case named "anchored" passes on
			// any refusal that quotes a file.
			if err == nil || !strings.Contains(strings.ReplaceAll(err.Error(), f.l.root, "<root>"), tc.reason) {
				t.Fatalf("want a refusal mentioning %q, got %v", tc.reason, err)
			}
			sameState(t, "after the refusal", f.state(t), original)
		})
	}
}

// A shadow the plan cannot see is still caught: the reload after writing is the
// net, and it restores the original bytes.
func TestImageWriteReloadRestoresWhatThePlanCouldNotSee(t *testing.T) {
	f := newWriteFixture(t, map[string]string{
		"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n  worker:\n    image: busybox\n    environment:\n      COPY: ${TW_IMG}\n",
		".env":         "TW_IMG=" + oldImg + "\n",
	}, ProjectEntry{})
	original := f.state(t)
	_, err := f.write(map[string]string{"app": newImg})
	if err == nil || !strings.Contains(err.Error(), "original files are restored") {
		t.Fatalf("want a restored refusal, got %v", err)
	}
	sameState(t, "after the reload refused the write", f.state(t), original)
}

// In-place means in place: line endings, quotes, comments, other documents and
// other assignments keep their bytes.
func TestImageWriteChangesOnlyTheToken(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		targets map[string]string
		want    map[string]string // expected content of changed files
	}{
		{"H1: irregular formatting, comments and blank lines",
			map[string]string{"compose.yaml": "services:\n\n  app:   # the app\n     image: " + oldImg + "   # pinned\n\n  db:\n     image: postgres:16\n"},
			map[string]string{"app": newImg},
			map[string]string{"compose.yaml": "services:\n\n  app:   # the app\n     image: " + newImg + "   # pinned\n\n  db:\n     image: postgres:16\n"}},
		{"S8: CRLF compose file",
			map[string]string{"compose.yaml": "services:\r\n  app:\r\n    image: '" + oldImg + "'\r\n"},
			map[string]string{"app": newImg},
			map[string]string{"compose.yaml": "services:\r\n  app:\r\n    image: '" + newImg + "'\r\n"}},
		{"S8: CRLF env file",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n", ".env": "A=1\r\nTW_IMG=" + oldImg + "\r\nB=2\r\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "A=1\r\nTW_IMG=" + newImg + "\r\nB=2\r\n"}},
		{"S8: CRLF env file gets a CRLF append",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG:-" + oldImg + "}\n", ".env": "A=1\r\nB=2"},
			map[string]string{"app": newImg},
			map[string]string{".env": "A=1\r\nB=2\r\nTW_IMG=" + newImg + "\r\n"}},
		{"S9: double quotes and an inline comment",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n", ".env": "TW_IMG=\"" + oldImg + "\" # pinned by hand\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "TW_IMG=\"" + newImg + "\" # pinned by hand\n"}},
		{"S9: single quotes",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n", ".env": "TW_IMG='" + oldImg + "'\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "TW_IMG='" + newImg + "'\n"}},
		{"S9: unquoted with an inline comment",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n", ".env": "TW_IMG=" + oldImg + "   # pinned\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "TW_IMG=" + newImg + "   # pinned\n"}},
		{"M7 / S11: a KEY= line inside another variable's quoted value is not an assignment",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n",
				".env": "NOTE=\"first\nTW_IMG=reg.example/evil:1\nlast\"\nTW_IMG=" + oldImg + "\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "NOTE=\"first\nTW_IMG=reg.example/evil:1\nlast\"\nTW_IMG=" + newImg + "\n"}},
		{"a byte order mark",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n", ".env": "\uFEFFTW_IMG=" + oldImg + "\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "\uFEFFTW_IMG=" + newImg + "\n"}},
		{"H3 / S3: a later document overrides the image",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: reg.example/app:base\n---\nservices:\n  app:\n    image: " + oldImg + "\n"},
			map[string]string{"app": newImg},
			map[string]string{"compose.yaml": "services:\n  app:\n    image: reg.example/app:base\n---\nservices:\n  app:\n    image: " + newImg + "\n"}},
		{"H3 / S4: the service lives in the second document",
			map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n---\nservices:\n  app:\n    image: " + oldImg + "\n"},
			map[string]string{"app": newImg},
			map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n---\nservices:\n  app:\n    image: " + newImg + "\n"}},
		{"H4 / S1: registry and tag variables — only the tag changes",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ${TW_REG}/app:${TW_TAG}\n", ".env": "TW_REG=reg.example\nTW_TAG=v1\n"},
			map[string]string{"app": newImg},
			map[string]string{".env": "TW_REG=reg.example\nTW_TAG=v2\n"}},
		{"H4: immich-style default tag variable",
			map[string]string{"compose.yaml": "services:\n  app:\n    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION:-release}\n", ".env": "UPLOAD=/x\n"},
			map[string]string{"app": "ghcr.io/immich-app/immich-server:v2.1.0"},
			map[string]string{".env": "UPLOAD=/x\nIMMICH_VERSION=v2.1.0\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWriteFixture(t, tc.files, ProjectEntry{})
			original := f.state(t)
			w, err := f.write(tc.targets)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			for k, v := range original {
				want[k] = v
			}
			for k, v := range tc.want {
				want[k] = v
			}
			sameState(t, "after the write", f.state(t), want)
			for svc, img := range tc.targets {
				if got := f.image(t, svc); got != img {
					t.Errorf("%s resolves %q, want %q", svc, got, img)
				}
			}
			if err := w.revert(context.Background(), nil, f.l.cb.loadProject); err != nil {
				t.Fatal(err)
			}
			sameState(t, "after the revert", f.state(t), original)
		})
	}
}

// M8 / S10: two variables appended to a .env the write created. The revert used to
// track existence per variable and restore in map order, leaving an empty .env in
// 37 of 40 runs. Bytes, not values: the file goes, every time.
func TestImageWriteRevertRestoresBytesEveryTime(t *testing.T) {
	files := map[string]string{"compose.yaml": "services:\n" +
		"  a:\n    image: ${TW_A:-reg.example/a:v1}\n" +
		"  b:\n    image: ${TW_B:-reg.example/b:v1}\n" +
		"  c:\n    image: ${TW_C:-reg.example/c:v1}\n"}
	targets := map[string]string{"a": "reg.example/a:v2", "b": "reg.example/b:v2", "c": "reg.example/c:v2"}
	for run := range 40 {
		f := newWriteFixture(t, files, ProjectEntry{})
		original := f.state(t)
		w, err := f.write(targets)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.revert(context.Background(), nil, f.l.cb.loadProject); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(f.dir, ".env")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("run %d: the .env the write created is still there (%v)", run, err)
		}
		sameState(t, "after the revert", f.state(t), original)
	}
}

// A per-service revert rebuilds each file from its original bytes with only the
// kept services' edits: never by writing an old value back.
func TestImageWritePartialRevert(t *testing.T) {
	f := newWriteFixture(t, map[string]string{"compose.yaml": "services:\n" +
		"  a:\n    image: ${TW_A:-reg.example/a:v1}\n" +
		"  b:\n    image: ${TW_B:-reg.example/b:v1}\n" +
		"  c:\n    image: reg.example/c:v1 # literal\n"}, ProjectEntry{})
	original := f.state(t)
	w, err := f.write(map[string]string{"a": "reg.example/a:v2", "b": "reg.example/b:v2", "c": "reg.example/c:v2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.revert(context.Background(), []string{"a", "c"}, f.l.cb.loadProject); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"compose.yaml": original["compose.yaml"], ".env": "TW_B=reg.example/b:v2\n"}
	sameState(t, "after reverting a and c", f.state(t), want)
	for svc, img := range map[string]string{"a": "reg.example/a:v1", "b": "reg.example/b:v2", "c": "reg.example/c:v1"} {
		if got := f.image(t, svc); got != img {
			t.Errorf("%s resolves %q, want %q", svc, got, img)
		}
	}

	// A file someone changed after the write is not clobbered by the revert.
	// (Checked on a fresh write below; first, the shared-edit rule.)
	shared := newWriteFixture(t, map[string]string{
		"compose.yaml": "services:\n  server:\n    image: ${TW_REL:-reg.example/app:v1}\n  worker:\n    image: ${TW_REL:-reg.example/app:v1}\n",
	}, ProjectEntry{})
	sw, err := shared.write(map[string]string{"server": "reg.example/app:v2", "worker": "reg.example/app:v2"})
	must(t, err)
	written := shared.state(t)
	err = sw.revert(context.Background(), []string{"server"}, shared.l.cb.loadProject)
	if err == nil || !strings.Contains(err.Error(), "server keeps its new image: it shares an edit") {
		t.Fatalf("reverting one of two services sharing an edit must keep it and say so, got %v", err)
	}
	sameState(t, "after reverting one sharer", shared.state(t), written)
	if got := shared.image(t, "worker"); got != "reg.example/app:v2" {
		t.Errorf("the service that keeps its image lost it: %q", got)
	}

	must(t, os.WriteFile(filepath.Join(f.dir, ".env"), []byte("TW_B=reg.example/b:hand-edited\n"), 0o600))
	err = w.revert(context.Background(), []string{"b"}, f.l.cb.loadProject)
	if err == nil || !strings.Contains(err.Error(), "changed after the update wrote it") {
		t.Fatalf("want the hand edit reported, got %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(f.dir, ".env")); string(got) != "TW_B=reg.example/b:hand-edited\n" {
		t.Errorf("the revert clobbered a hand edit: %q", got)
	}
}

// H2: jobs on one project take its lock, so concurrent image writes with the real
// writer never interleave — every write verifies, and the last one stands.
func TestConcurrentImageWritesOnOneProjectAreSerialised(t *testing.T) {
	f := newWriteFixture(t, map[string]string{
		"compose.yaml": "services:\n  app:\n    image: ${TW_IMG}\n  side:\n    image: ${TW_SIDE}\n",
		".env":         "TW_IMG=reg.example/app:v0\nTW_SIDE=reg.example/side:v0\n",
	}, ProjectEntry{})
	locks := newProjectLocks()
	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	for writer := range 4 {
		wg.Go(func() {
			for i := range 15 {
				release, err := locks.acquire(context.Background(), f.entry.Name, "writer", nil)
				if err != nil {
					t.Error(err)
					return
				}
				if inside.Add(1) > 1 {
					overlaps.Add(1)
				}
				svc, img := "app", "reg.example/app:w"+string(rune('a'+writer))
				if i%2 == 1 {
					svc, img = "side", "reg.example/side:w"+string(rune('a'+writer))
				}
				if _, err := f.write(map[string]string{svc: img + string(rune('0'+i%10))}); err != nil {
					t.Errorf("writer %d, write %d: %v", writer, i, err)
				}
				inside.Add(-1)
				release()
			}
		})
	}
	wg.Wait()
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d writes ran while another held the project", n)
	}
	stmts, err := scanEnvStatements([]byte(f.state(t)[".env"]))
	if err != nil || len(stmts) != 2 {
		t.Fatalf("the .env is torn: %d statements, %v:\n%s", len(stmts), err, f.state(t)[".env"])
	}
}

func TestProjectLockHonoursCancellationAndDropsReleasedLocks(t *testing.T) {
	l := newProjectLocks()
	release, err := l.acquire(context.Background(), "p", "job one", nil)
	must(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	var told string
	done := make(chan error, 1)
	go func() {
		_, err := l.acquire(ctx, "p", "job two", func(h string) { told = h })
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait must give up, got %v", err)
	}
	if told != "job one" {
		t.Errorf("the waiter was told %q holds the lock", told)
	}

	other, err := l.acquire(context.Background(), "q", "job three", nil)
	must(t, err) // another project is independent
	other()

	got := make(chan struct{})
	go func() {
		r, err := l.acquire(context.Background(), "p", "job four", nil)
		if err == nil {
			r()
		}
		close(got)
	}()
	release()
	release() // idempotent
	<-got
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.locks) != 0 {
		t.Errorf("released locks are not dropped: %d left", len(l.locks))
	}
}

// Replacing a file's content must not replace anything else about it, and must
// not be steerable by what sits at a temp name.
func TestWriteFileAtomicChangesOnlyContent(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.env")
	must(t, os.WriteFile(shared, []byte("A=1\n"), 0o640))
	must(t, os.Chmod(shared, 0o640))
	groups, err := os.Getgroups()
	must(t, err)
	otherGroup := -1
	for _, g := range groups {
		if g != os.Getgid() {
			otherGroup = g
			break
		}
	}
	if otherGroup < 0 {
		t.Skip("needs a user in a second group to observe the owner being kept")
	}
	must(t, os.Lchown(shared, os.Getuid(), otherGroup))
	link := filepath.Join(dir, ".env")
	must(t, os.Symlink("shared.env", link))

	// M9: a symlink planted at the old predictable temp name, pointing outside.
	outside := filepath.Join(t.TempDir(), "victim")
	must(t, os.WriteFile(outside, []byte("untouched\n"), 0o600))
	must(t, os.Symlink(outside, filepath.Join(dir, ".shared.env.tmp")))

	before, err := os.Stat(shared)
	must(t, err)
	must(t, writeFileAtomic(link, []byte("A=2\n"), 0o600))

	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a file (err=%v)", err)
	}
	if got, _ := os.ReadFile(shared); string(got) != "A=2\n" {
		t.Errorf("the linked file reads %q", got)
	}
	if got, _ := os.ReadFile(outside); string(got) != "untouched\n" {
		t.Errorf("🔴 the write followed a planted temp symlink: %q", got)
	}
	after, err := os.Stat(shared)
	must(t, err)
	if after.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want the file's own 0640", after.Mode().Perm())
	}
	bo, _ := statOwner(before)
	ao, _ := statOwner(after)
	if bo.Uid != ao.Uid || bo.Gid != ao.Gid {
		t.Errorf("owner %d:%d, want %d:%d", ao.Uid, ao.Gid, bo.Uid, bo.Gid)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	// W6: a directory at the old temp name made the write fail, and its cleanup
	// then removed the directory.
	plain := filepath.Join(dir, "plain.env")
	must(t, os.WriteFile(plain, []byte("C=1\n"), 0o600))
	must(t, os.Mkdir(filepath.Join(dir, ".plain.env.tmp"), 0o755))
	must(t, writeFileAtomic(plain, []byte("C=2\n"), 0o600))
	if fi, err := os.Stat(filepath.Join(dir, ".plain.env.tmp")); err != nil || !fi.IsDir() {
		t.Errorf("a planted directory was removed (err=%v)", err)
	}
	if got, _ := os.ReadFile(plain); string(got) != "C=2\n" {
		t.Errorf("the write beside a planted directory reads %q", got)
	}

	created := filepath.Join(dir, "new.env")
	must(t, writeFileAtomic(created, []byte("B=1\n"), 0o600))
	if fi, err := os.Stat(created); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a created file takes the given mode: %v, err=%v", fi, err)
	}
}

// Which image templates are written, and through which variable. Everything not
// listed as a whole-value or trailing-tag variable is refused.
func TestImageTemplateClassification(t *testing.T) {
	cases := []struct {
		raw          string
		whole, trail string // the variable each form yields; "" = not that form
	}{
		{"${TRAEFIK_IMAGE}", "TRAEFIK_IMAGE", ""},
		{"$PLAIN", "PLAIN", ""},
		{"${VAR:-reg/app:v1}", "VAR", ""},
		{"${VAR-reg/app:v1}", "VAR", ""},
		{"${VAR:?set it}", "VAR", ""},
		{"${VAR:+alt}", "", ""},
		{"${VAR:-${OTHER}}", "", ""},
		{"reg/app:${TAG}", "", "TAG"},
		{"reg/app:${TAG:-release}", "", "TAG"},
		{"${REG}/app:${TAG}", "", "TAG"},
		{"reg:5000/app:$TAG", "", "TAG"},
		{"${REGISTRY:-docker.io}/library/traefik:v3.1", "", ""},
		{"reg/app:${TAG}-alpine", "", ""},
		{"reg/app:v1@${DIGEST}", "", ""},
		{"reg/${NAME}:v1", "", ""},
		{"ghcr.io/foo/bar:1.2.3", "", ""},
	}
	for _, c := range cases {
		whole, _ := wholeVarRef(c.raw)
		trail, _ := trailingTagVar(c.raw)
		if whole != c.whole || trail != c.trail {
			t.Errorf("%q: whole=%q trailing=%q, want %q %q", c.raw, whole, trail, c.whole, c.trail)
		}
	}
}

// The scanner is compose-go's dotenv parser with offsets kept: on every file the
// two must agree on the keys, and — where no expansion applies — on the values.
func TestEnvScanAgreesWithComposeDotenv(t *testing.T) {
	files := []string{
		"A=1\nB=two\n",
		"export A=1\nexport   B=2\n",
		"A: 1\nB:two\n",
		"A=\"quoted value\"\nB='single'\n",
		"A=\"line one\nB=not an assignment\nline three\"\nC=3\n",
		"# comment\n\n   # indented comment\nA=1 # inline\nB=2#not-a-comment\n",
		"A=1\r\nB=2\r\n",
		"\uFEFFA=1\n",
		"A=\nB= \nC=\"\"\n",
		"  A  =  spaced  \n",
		"A\nB=1\n",
		"dotted.key=1\ndashed-key=2\nbracket[0]=3\n",
		"A=1",
		"LONE",
		"A=\"escaped \\\" quote\"\nB=2\n",
		"A='it''s'\n",
		"A=1\nA=2\nexport A=3\n",
	}
	for _, src := range files {
		stmts, err := scanEnvStatements([]byte(src))
		// ParseWithLookup, not UnmarshalBytes: it is the path GetEnvFromFile takes,
		// and the one that seeks past a byte order mark.
		want, werr := dotenv.ParseWithLookup(strings.NewReader(src), func(k string) (string, bool) { return "<inherited:" + k + ">", true })
		if (err == nil) != (werr == nil) {
			t.Errorf("%q: scanner err %v, compose err %v", src, err, werr)
			continue
		}
		if err != nil {
			continue
		}
		got := map[string]string{}
		for _, st := range stmts {
			if st.inherited {
				got[st.key] = "<inherited:" + st.key + ">"
				continue
			}
			got[st.key] = src[st.valueStart:st.valueEnd]
		}
		if len(got) != len(want) {
			t.Errorf("%q: scanner keys %v, compose keys %v", src, got, want)
			continue
		}
		for k, v := range want {
			g, ok := got[k]
			if !ok {
				t.Errorf("%q: compose sets %q, the scanner does not see it", src, k)
				continue
			}
			if strings.ContainsAny(g, "\\$") {
				continue // escapes and interpolation are compose's to expand
			}
			if g != v {
				t.Errorf("%q: %s = %q, compose reads %q", src, k, g, v)
			}
		}
	}
	for _, bad := range []string{"A=\"unterminated\n", "A B=1\n", "A!=1\n"} {
		if _, err := scanEnvStatements([]byte(bad)); err == nil {
			t.Errorf("%q: compose refuses it, the scanner did not", bad)
		}
		if _, err := dotenv.ParseWithLookup(strings.NewReader(bad), nil); err == nil {
			t.Errorf("%q: fixture: compose accepts it", bad)
		}
	}
}

// A write that fails partway puts back the files it had already written: on any
// error, no file differs from what it was.
func TestImageWriteFailingPartwayRestoresWhatItWrote(t *testing.T) {
	f := newWriteFixture(t, map[string]string{
		"ro/a.yml": "services:\n  a:\n    image: reg.example/a:v1\n",
		"b.yml":    "services:\n  b:\n    image: reg.example/b:v1\n",
	}, ProjectEntry{ComposeFiles: []string{"ro/a.yml", "b.yml"}})
	original := f.state(t)
	ro := filepath.Join(f.dir, "ro")
	must(t, os.Chmod(ro, 0o555))
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if probe, err := os.CreateTemp(ro, "probe"); err == nil {
		probe.Close()
		t.Skip("running as a user a read-only directory does not stop")
	}

	_, err := f.write(map[string]string{"a": "reg.example/a:v2", "b": "reg.example/b:v2"})
	if err == nil {
		t.Fatal("the write into a read-only directory succeeded")
	}
	sameState(t, "after a failed write", f.state(t), original)
}
