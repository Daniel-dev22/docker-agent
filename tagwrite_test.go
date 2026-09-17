package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/cli"
)

// composeImage is the image compose itself resolves for service — its own file
// discovery, env-file order and interpolation, independent of resolveLoadPaths.
// Relative entry paths are relative to the working dir, as the registry stores them.
func composeImage(t *testing.T, e ProjectEntry, service string) string {
	t.Helper()
	abs := func(ps []string) []string {
		var out []string
		for _, p := range ps {
			out = append(out, filepath.Join(e.WorkingDir, p))
		}
		return out
	}
	fns := []cli.ProjectOptionsFn{
		cli.WithWorkingDirectory(e.WorkingDir),
		cli.WithOsEnv,
		cli.WithEnvFiles(abs(e.EnvFiles)...),
		cli.WithDotEnv,
		cli.WithName("p"),
	}
	if len(e.ComposeFiles) == 0 {
		fns = append(fns, cli.WithDefaultConfigPath)
	}
	opts, err := cli.NewProjectOptions(abs(e.ComposeFiles), fns...)
	if err != nil {
		t.Fatal(err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return project.Services[service].Image
}

// dirState is every file in dir with its bytes.
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, de := range entries {
		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		state[de.Name()] = string(data)
	}
	return state
}

// An update must write the image to the file compose takes it from — so the tag
// deployed and the tag the next `compose up` reads agree — and a revert must put
// every file back byte for byte, removing what the update added.
func TestTagWritesFollowComposeResolution(t *testing.T) {
	const svc, newImage = "app", "reg/app:v2"
	cases := []struct {
		name  string
		files map[string]string
		entry ProjectEntry
		// written is the one file the update must change.
		written, before string
	}{
		{
			name: "default discovery: the override's literal image wins",
			files: map[string]string{
				"compose.yaml":          "services:\n  app:\n    image: reg/app:base\n",
				"compose.override.yaml": "services:\n  app:\n    image: reg/app:v1\n",
			},
			written: "compose.override.yaml", before: "reg/app:v1",
		},
		{
			name: "declared compose files: the last one setting the image wins",
			files: map[string]string{
				"a.yml": "services:\n  app:\n    image: reg/app:base\n",
				"b.yml": "services:\n  app:\n    image: reg/app:v1\n",
				"c.yml": "services:\n  app:\n    environment:\n      X: y\n",
			},
			entry:   ProjectEntry{ComposeFiles: []string{"a.yml", "b.yml", "c.yml"}},
			written: "b.yml", before: "reg/app:v1",
		},
		{
			name: "env files: the last one declaring the var wins",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TAGWRITE_IMAGE}\n",
				"one.env":      "TAGWRITE_IMAGE=reg/app:base\n",
				"two.env":      "TAGWRITE_IMAGE=reg/app:v1\n",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "two.env", before: "reg/app:v1",
		},
		{
			name: "env files: a later file that does not declare the var is not the one",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TAGWRITE_IMAGE}\n",
				"one.env":      "TAGWRITE_IMAGE=reg/app:v1\n",
				"two.env":      "OTHER=x\n",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "one.env", before: "reg/app:v1",
		},
		{
			name: "a bare KEY inheriting an earlier file's value declares it",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TAGWRITE_IMAGE}\n",
				"one.env":      "TAGWRITE_IMAGE=reg/app:v1\n",
				"two.env":      "TAGWRITE_IMAGE\n",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "two.env", before: "reg/app:v1",
		},
		{
			name: "export form: the appended plain line takes effect",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TAGWRITE_IMAGE}\n",
				".env":         "export TAGWRITE_IMAGE=reg/app:v1\nOTHER=x",
			},
			written: ".env", before: "reg/app:v1",
		},
		{
			name: "undeclared var with explicit env files: the last env file",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TAGWRITE_IMAGE-reg/app:v1}\n",
				"one.env":      "OTHER=x\n",
				"two.env":      "",
			},
			entry:   ProjectEntry{EnvFiles: []string{"one.env", "two.env"}},
			written: "two.env", before: "reg/app:v1",
		},
		{
			name: "undeclared var, no env file: the working dir's .env, removed on revert",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    image: ${TAGWRITE_IMAGE-reg/app:v1}\n",
			},
			written: ".env", before: "reg/app:v1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			entry := tc.entry
			entry.Name, entry.WorkingDir = "p", dir
			if got := composeImage(t, entry, svc); got != tc.before {
				t.Fatalf("fixture: compose resolves %q, want %q", got, tc.before)
			}
			original := dirState(t, dir)

			prior, err := setServiceImage(entry, svc, newImage)
			if err != nil {
				t.Fatal(err)
			}
			if got := composeImage(t, entry, svc); got != newImage {
				t.Errorf("after the update compose resolves %q, want %q", got, newImage)
			}
			after := dirState(t, dir)
			for name, body := range after {
				if name == tc.written {
					if body == original[name] {
						t.Errorf("%s was not written", name)
					}
				} else if body != original[name] {
					t.Errorf("%s changed, but compose takes the image from %s", name, tc.written)
				}
			}

			if err := restoreServiceImage(entry, svc, prior); err != nil {
				t.Fatal(err)
			}
			restored := dirState(t, dir)
			if len(restored) != len(original) {
				t.Errorf("revert left files %v, want %v", keys(restored), keys(original))
			}
			for name, body := range original {
				if restored[name] != body {
					t.Errorf("revert: %s = %q, want %q", name, restored[name], body)
				}
			}
			if got := composeImage(t, entry, svc); got != tc.before {
				t.Errorf("after the revert compose resolves %q, want %q", got, tc.before)
			}
		})
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A literal image is rewritten in its token: every other byte of the file —
// indentation, blank lines, comments, quoting, CRLF — is what it was. A token that
// cannot be rewritten in place is re-encoded, and must still read the new image.
func TestLiteralImageRewriteTouchesOnlyItsToken(t *testing.T) {
	const newImage = "reg/app:v2@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name, before, after string // after "" = re-encoded; only the value is checked
		image               string // the value written; newImage when ""
	}{
		{"plain, comment and blank lines",
			"services:\n\n  app:   # the app\n     image: reg/app:v1   # pinned\n\n  db:\n     image: pg:16\n",
			"services:\n\n  app:   # the app\n     image: " + newImage + "   # pinned\n\n  db:\n     image: pg:16\n", ""},
		{"single-quoted",
			"services:\n  app:\n    image: 'reg/app:v1'\n",
			"services:\n  app:\n    image: '" + newImage + "'\n", ""},
		{"double-quoted in a flow mapping after a multibyte key",
			"services: {app: {labels: {é: x}, image: \"reg/app:v1\"}}\n",
			"services: {app: {labels: {é: x}, image: \"" + newImage + "\"}}\n", ""},
		{"CRLF line endings",
			"services:\r\n  app:\r\n    image: reg/app:v1\r\n",
			"services:\r\n  app:\r\n    image: " + newImage + "\r\n", ""},
		{"a tagged scalar is re-encoded", "services:\n  app:\n    image: !!str reg/app:v1\n", "", ""},
		{"a block scalar is re-encoded", "services:\n  app:\n    image: >-\n      reg/app:v1\n", "", ""},
		{"an escaped double-quoted scalar is re-encoded", "services:\n  app:\n    image: \"reg/app:\\x761\"\n", "", ""},
		// Spelled in place as a plain scalar this would be a mapping indicator, not
		// the value: the result is re-parsed, and re-encoded when it does not read v.
		{"a value that changes meaning in place is re-encoded", "services:\n  app:\n    image: reg/app:v1\n", "", "reg/app:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compose.yaml")
			if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			raw, found, _, setScalar, err := findServiceImage(path, "app")
			if err != nil || !found || raw != "reg/app:v1" {
				t.Fatalf("fixture: raw=%q found=%v err=%v", raw, found, err)
			}
			image := newImage
			if tc.image != "" {
				image = tc.image
			}
			if err := setScalar(image); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if tc.after != "" && string(got) != tc.after {
				t.Errorf("rewrite changed more than the token:\n got %q\nwant %q", got, tc.after)
			}
			if v, found, _, _, err := imageIn(got, "app"); err != nil || !found || v != image {
				t.Errorf("rewritten file reads image %q (found=%v err=%v), want %q", v, found, err, image)
			}
		})
	}
}

// Replacing a file's content must not replace anything else about it: a symlinked
// env file stays a symlink to the same file, and the file keeps its mode.
func TestWriteFileAtomicChangesOnlyContent(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared.env")
	if err := os.WriteFile(shared, []byte("A=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o640); err != nil {
		t.Fatal(err)
	}
	// A group other than the one a new file gets, so a write that does not carry
	// the owner over shows. A non-root user can hand a file only to its own groups.
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
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
	if err := os.Lchown(shared, os.Getuid(), otherGroup); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink("shared.env", link); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(link, []byte("A=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a file (err=%v)", err)
	}
	if got, _ := os.ReadFile(shared); string(got) != "A=2\n" {
		t.Errorf("the linked file reads %q, want the new content", got)
	}
	after, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want the file's own 0640", after.Mode().Perm())
	}
	bo, _ := statOwner(before)
	ao, _ := statOwner(after)
	if bo.Uid != ao.Uid || bo.Gid != ao.Gid {
		t.Errorf("owner %d:%d, want %d:%d", ao.Uid, ao.Gid, bo.Uid, bo.Gid)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("temp file left behind: %v", entries)
	}

	created := filepath.Join(dir, "new.env")
	if err := writeFileAtomic(created, []byte("B=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(created); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("a created file takes the given mode: %v, err=%v", fi, err)
	}
}
