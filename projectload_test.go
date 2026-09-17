package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

const loadSecret = "SECRET-TOKEN-7f3a"

// loadEnv is a real compose backend (compose-go loader, remote loaders) over a
// temp compose root, and a secret file outside it.
type loadEnv struct {
	cb      *composeBackend
	root    string
	outside string // a directory outside the root
	secret  string // a file in it
}

func newLoadEnv(t *testing.T) *loadEnv {
	t.Helper()
	eng := newFakeEngine(t)
	root := t.TempDir()
	cb, err := newComposeBackend(Config{DockerHost: eng.host(), ComposeRoot: root})
	must(t, err)
	outside := t.TempDir()
	secret := filepath.Join(outside, "bearer-token")
	must(t, os.WriteFile(secret, []byte(loadSecret+"\n"), 0o600))
	return &loadEnv{cb: cb, root: root, outside: outside, secret: secret}
}

func (l *loadEnv) project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(l.root, name)
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o644))
	}
	must(t, os.MkdirAll(dir, 0o755))
	return dir
}

// refused asserts the load was refused by confinement and quotes no content.
func refused(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "outside docker-agent's compose root") {
		t.Fatalf("want a confinement refusal, got %v", err)
	}
	if strings.Contains(err.Error(), loadSecret) {
		t.Fatalf("the refusal quotes the file: %v", err)
	}
}

func TestProjectLoadIsConfined(t *testing.T) {
	ctx := context.Background()
	l := newLoadEnv(t)

	t.Run("relative-env-file-resolves-in-the-working-dir", func(t *testing.T) {
		dir := l.project(t, "relenv", map[string]string{
			"docker-compose.yml": "services:\n  app:\n    image: ${IMG}\n",
			"conf/stack.env":     "IMG=registry.example.invalid/app:1.2.3\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "relenv", WorkingDir: dir, EnvFiles: []string{"conf/stack.env"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Services["app"].Image; got != "registry.example.invalid/app:1.2.3" {
			t.Fatalf("relative env file not read from the working dir: image %q", got)
		}
	})
	t.Run("relative-env-file-escaping-the-root", func(t *testing.T) {
		dir := l.project(t, "escenv", map[string]string{"docker-compose.yml": "services:\n  app:\n    image: alpine\n"})
		rel, err := filepath.Rel(dir, l.secret)
		must(t, err)
		_, err = l.cb.loadProject(ctx, ProjectEntry{Name: "escenv", WorkingDir: dir, EnvFiles: []string{rel}})
		refused(t, err)
	})
	t.Run("include-outside-absolute", func(t *testing.T) {
		must(t, os.WriteFile(filepath.Join(l.outside, "inc.yaml"), []byte("services:\n  leak:\n    image: "+loadSecret+"\n"), 0o644))
		dir := l.project(t, "incabs", map[string]string{
			"compose.yaml": "include:\n  - " + filepath.Join(l.outside, "inc.yaml") + "\nservices:\n  app:\n    image: alpine\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incabs", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("include-outside-relative", func(t *testing.T) {
		dir := l.project(t, "increl", map[string]string{"compose.yaml": "services:\n  app:\n    image: alpine\n"})
		rel, err := filepath.Rel(dir, filepath.Join(l.outside, "inc.yaml"))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("include:\n  - "+rel+"\nservices:\n  app:\n    image: alpine\n"), 0o644))
		_, err = l.cb.loadProject(ctx, ProjectEntry{Name: "increl", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("include-inside-loads", func(t *testing.T) {
		dir := l.project(t, "incok", map[string]string{
			"compose.yaml":     "include:\n  - lib/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"lib/compose.yaml": "services:\n  sidecar:\n    image: busybox\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incok", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.Services["sidecar"]; !ok {
			t.Fatal("a confined include must still load")
		}
	})
	t.Run("included-project-dotenv-symlinked-out", func(t *testing.T) {
		dir := l.project(t, "incdotenv", map[string]string{
			"compose.yaml":     "include:\n  - lib/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"lib/compose.yaml": "services:\n  sidecar:\n    image: busybox\n",
		})
		must(t, os.Symlink(l.secret, filepath.Join(dir, "lib", ".env")))
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incdotenv", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("extends-outside", func(t *testing.T) {
		must(t, os.WriteFile(filepath.Join(l.outside, "base.yaml"), []byte("services:\n  base:\n    image: "+loadSecret+"\n"), 0o644))
		dir := l.project(t, "extabs", map[string]string{
			"compose.yaml": "services:\n  app:\n    extends:\n      file: " + filepath.Join(l.outside, "base.yaml") + "\n      service: base\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "extabs", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("extends-inside-loads", func(t *testing.T) {
		dir := l.project(t, "extok", map[string]string{
			"compose.yaml": "services:\n  app:\n    extends:\n      file: common.yaml\n      service: base\n",
			"common.yaml":  "services:\n  base:\n    image: busybox:1.36\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "extok", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["app"].Image != "busybox:1.36" {
			t.Fatalf("a confined extends must still load: %+v", p.Services["app"])
		}
	})
	t.Run("service-env-file-outside", func(t *testing.T) {
		dir := l.project(t, "svcenv", map[string]string{
			"compose.yaml": "services:\n  app:\n    image: alpine\n    env_file: " + l.secret + "\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "svcenv", WorkingDir: dir})
		refused(t, err)
	})
	// Compose's dotenv parser quotes a line it cannot parse: the refusal must come
	// before any read, not after a load error has already quoted the secret.
	unparsable := filepath.Join(l.outside, "unparsable")
	must(t, os.WriteFile(unparsable, []byte(loadSecret+" !@#\n"), 0o600))
	t.Run("service-env-file-outside-unparsable", func(t *testing.T) {
		dir := l.project(t, "svcenvbad", map[string]string{
			"compose.yaml": "services:\n  app:\n    image: alpine\n    env_file: " + unparsable + "\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "svcenvbad", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("service-label-file-outside-unparsable", func(t *testing.T) {
		for i, form := range []string{"label_file: " + unparsable, "label_file:\n      - " + unparsable} {
			name := fmt.Sprintf("svclabel%d", i)
			dir := l.project(t, name, map[string]string{
				"compose.yaml": "services:\n  app:\n    image: alpine\n    " + form + "\n",
			})
			_, err := l.cb.loadProject(ctx, ProjectEntry{Name: name, WorkingDir: dir})
			refused(t, err)
		}
	})
	t.Run("service-label-file-inside-is-applied", func(t *testing.T) {
		dir := l.project(t, "svclabelok", map[string]string{
			"compose.yaml": "services:\n  app:\n    image: alpine\n    label_file: app.labels\n",
			"app.labels":   "com.example.team=platform\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "svclabelok", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["app"].Labels["com.example.team"] != "platform" {
			t.Fatalf("a confined label_file must still apply: %v", p.Services["app"].Labels)
		}
	})
	t.Run("service-env-file-inside-is-resolved", func(t *testing.T) {
		dir := l.project(t, "svcenvok", map[string]string{
			"compose.yaml": "services:\n  app:\n    image: alpine\n    env_file: app.env\n",
			"app.env":      "GREETING=hello\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "svcenvok", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if v := p.Services["app"].Environment["GREETING"]; v == nil || *v != "hello" {
			t.Fatalf("the deferred environment resolution must still run: %v", p.Services["app"].Environment)
		}
	})
	t.Run("symlinked-override-file", func(t *testing.T) {
		dir := l.project(t, "override", map[string]string{"compose.yaml": "services:\n  app:\n    image: alpine\n"})
		must(t, os.Symlink(l.secret, filepath.Join(dir, "compose.override.yaml")))
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "override", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("real-override-file-still-applies", func(t *testing.T) {
		dir := l.project(t, "overrideok", map[string]string{
			"compose.yaml":          "services:\n  app:\n    image: alpine\n",
			"compose.override.yaml": "services:\n  app:\n    image: alpine:3.20\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "overrideok", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["app"].Image != "alpine:3.20" {
			t.Fatalf("the override was not applied: %q", p.Services["app"].Image)
		}
	})
	// Include processing reads an include's env_file and stats its project_directory
	// with no loader or listener; the pre-load include scan must refuse first.
	must(t, os.WriteFile(filepath.Join(l.outside, "unparsable.env"), []byte(loadSecret+" !@#\n"), 0o600))
	t.Run("include-env-file-outside", func(t *testing.T) {
		for i, entry := range []string{
			"  - path: lib/compose.yaml\n    env_file: " + filepath.Join(l.outside, "unparsable.env"),
			"  - path: lib/compose.yaml\n    env_file:\n      - " + filepath.Join(l.outside, "unparsable.env"),
		} {
			name := fmt.Sprintf("incenv%d", i)
			dir := l.project(t, name, map[string]string{
				"compose.yaml":     "include:\n" + entry + "\nservices:\n  app:\n    image: alpine\n",
				"lib/compose.yaml": "services:\n  sidecar:\n    image: busybox\n",
			})
			_, err := l.cb.loadProject(ctx, ProjectEntry{Name: name, WorkingDir: dir})
			refused(t, err)
		}
	})
	t.Run("include-env-file-relative-escape", func(t *testing.T) {
		dir := l.project(t, "incenvrel", map[string]string{
			"compose.yaml":     "services:\n  app:\n    image: alpine\n",
			"lib/compose.yaml": "services:\n  sidecar:\n    image: busybox\n",
		})
		rel, err := filepath.Rel(dir, filepath.Join(l.outside, "unparsable.env"))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, "compose.yaml"),
			[]byte("include:\n  - path: lib/compose.yaml\n    env_file: "+rel+"\nservices:\n  app:\n    image: alpine\n"), 0o644))
		_, err = l.cb.loadProject(ctx, ProjectEntry{Name: "incenvrel", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("include-project-directory-outside", func(t *testing.T) {
		must(t, os.WriteFile(filepath.Join(l.outside, ".env"), []byte(loadSecret+" !@#\n"), 0o600))
		dir := l.project(t, "incpd", map[string]string{
			"compose.yaml":     "include:\n  - path: lib/compose.yaml\n    project_directory: " + l.outside + "\nservices:\n  app:\n    image: alpine\n",
			"lib/compose.yaml": "services:\n  sidecar:\n    image: busybox\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incpd", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("include-string-and-list-forms-outside", func(t *testing.T) {
		for i, entry := range []string{
			"  - " + filepath.Join(l.outside, "inc.yaml"),
			"  - path:\n      - lib/compose.yaml\n      - " + filepath.Join(l.outside, "inc.yaml"),
		} {
			name := fmt.Sprintf("incform%d", i)
			dir := l.project(t, name, map[string]string{
				"compose.yaml":     "include:\n" + entry + "\nservices:\n  app:\n    image: alpine\n",
				"lib/compose.yaml": "services:\n  sidecar:\n    image: busybox\n",
			})
			_, err := l.cb.loadProject(ctx, ProjectEntry{Name: name, WorkingDir: dir})
			refused(t, err)
		}
	})
	t.Run("nested-include-outside-at-depth-2", func(t *testing.T) {
		dir := l.project(t, "incdeep", map[string]string{
			"compose.yaml":     "include:\n  - a/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"a/compose.yaml":   "include:\n  - b/compose.yaml\nservices:\n  a:\n    image: busybox\n",
			"a/b/compose.yaml": "include:\n  - path: c.yaml\n    env_file: " + filepath.Join(l.outside, "unparsable.env") + "\nservices:\n  b:\n    image: busybox\n",
			"a/b/c.yaml":       "services:\n  c:\n    image: busybox\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incdeep", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("nested-include-inside-loads", func(t *testing.T) {
		dir := l.project(t, "incdeepok", map[string]string{
			"compose.yaml":     "include:\n  - a/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"a/compose.yaml":   "include:\n  - path: b/compose.yaml\n    env_file: " + filepath.Join(l.root, "incdeepok", "a", "b", "b.env") + "\nservices:\n  a:\n    image: busybox\n",
			"a/b/compose.yaml": "services:\n  b:\n    image: ${B_IMAGE}\n",
			"a/b/b.env":        "B_IMAGE=busybox:1.36\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incdeepok", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["b"].Image != "busybox:1.36" {
			t.Fatalf("a confined nested include with its env_file must still load: %+v", p.Services["b"])
		}
	})
	// compose resolves a nested include's relative env_file against the includer's
	// RELATIVE working dir — the process cwd. The scan must confine that file, not
	// the one a reader of the YAML would expect.
	t.Run("nested-relative-env-file-resolves-like-compose", func(t *testing.T) {
		cwd, err := os.Getwd()
		must(t, err)
		dir := l.project(t, "incquirk", map[string]string{
			"compose.yaml":     "include:\n  - a/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"a/compose.yaml":   "include:\n  - path: b/compose.yaml\n    env_file: quirk.env\nservices:\n  a:\n    image: busybox\n",
			"a/b/compose.yaml": "services:\n  b:\n    image: busybox\n",
			"a/quirk.env":      "X=1\n",
		})
		// compose opens <cwd>/a/quirk.env (outside the root), not <dir>/a/quirk.env.
		_, err = l.cb.loadProject(ctx, ProjectEntry{Name: "incquirk", WorkingDir: dir})
		if within(cwd, l.root) {
			t.Skip("test cwd is under the compose root")
		}
		refused(t, err)
	})
	t.Run("include-cycle-is-refused-without-hanging", func(t *testing.T) {
		dir := l.project(t, "inccycle", map[string]string{
			"compose.yaml":   "include:\n  - a/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"a/compose.yaml": "include:\n  - ../compose.yaml\nservices:\n  a:\n    image: busybox\n",
		})
		done := make(chan error, 1)
		go func() {
			_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "inccycle", WorkingDir: dir})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("got %v, want an include cycle refusal", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an include cycle hung the load")
		}
	})
	t.Run("include-depth-is-bounded", func(t *testing.T) {
		files := map[string]string{}
		dirPath := ""
		for i := range maxScanDepth + 2 {
			files[filepath.Join(dirPath, "compose.yaml")] = fmt.Sprintf("include:\n  - d/compose.yaml\nservices:\n  s%d:\n    image: busybox\n", i)
			dirPath = filepath.Join(dirPath, "d")
		}
		files[filepath.Join(dirPath, "compose.yaml")] = "services:\n  leaf:\n    image: busybox\n"
		dir := l.project(t, "incdepth", files)
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "incdepth", WorkingDir: dir})
		if err == nil || !strings.Contains(err.Error(), "nest deeper") {
			t.Fatalf("got %v, want the depth bound", err)
		}
	})
	// The real load's own guard, with the model pre-scan out of the way: each must
	// refuse on its own.
	// Two levels deep, `../../` resolves against the INCLUDED project's directory —
	// inside the stack — not the project's working dir.
	t.Run("deep-include-extends-loads", func(t *testing.T) {
		dir := l.project(t, "deepext", map[string]string{
			"compose.yaml":     "include:\n  - a/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"a/compose.yaml":   "include:\n  - b/compose.yaml\n",
			"a/b/compose.yaml": "services:\n  deep:\n    extends:\n      file: ../../base.yml\n      service: base\n",
			"base.yml":         "services:\n  base:\n    image: busybox:deep\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "deepext", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["deep"].Image != "busybox:deep" {
			t.Fatalf("deep extends not applied: %+v", p.Services["deep"])
		}
	})
	t.Run("deep-include-include-loads", func(t *testing.T) {
		dir := l.project(t, "deepinc", map[string]string{
			"compose.yaml":     "include:\n  - a/compose.yaml\nservices:\n  app:\n    image: alpine\n",
			"a/compose.yaml":   "include:\n  - b/compose.yaml\n",
			"a/b/compose.yaml": "include:\n  - ../../lib.yml\n",
			"lib.yml":          "services:\n  lib:\n    image: busybox:lib\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "deepinc", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["lib"].Image != "busybox:lib" {
			t.Fatalf("deep include not applied: %+v", p.Services)
		}
	})
	t.Run("chained-extends-loads", func(t *testing.T) {
		dir := l.project(t, "chain", map[string]string{
			"compose.yaml":          "services:\n  app:\n    extends:\n      file: bases/mid.yml\n      service: mid\n",
			"bases/mid.yml":         "services:\n  mid:\n    extends:\n      file: deeper/root.yml\n      service: root\n    environment:\n      MID: \"1\"\n",
			"bases/deeper/root.yml": "services:\n  root:\n    image: busybox:chain\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "chain", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if p.Services["app"].Image != "busybox:chain" {
			t.Fatalf("chained extends not applied: %+v", p.Services["app"])
		}
	})
	t.Run("extends-cycle-is-refused-without-hanging", func(t *testing.T) {
		dir := l.project(t, "extcycle", map[string]string{
			"compose.yaml": "services:\n  app:\n    extends:\n      file: a.yml\n      service: a\n",
			"a.yml":        "services:\n  a:\n    extends:\n      file: b.yml\n      service: b\n",
			"b.yml":        "services:\n  b:\n    extends:\n      file: a.yml\n      service: a\n",
		})
		done := make(chan error, 1)
		go func() {
			_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "extcycle", WorkingDir: dir})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("an extends cycle loaded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an extends cycle hung the load")
		}
	})
	// A reference that escapes from its exact base is refused, even beside a deep
	// include that — from some other base — would have kept it inside.
	must(t, os.WriteFile(filepath.Join(l.outside, "base.yml"), []byte("services:\n  base:\n    image: "+loadSecret+"\n"), 0o644))
	t.Run("extends-escaping-its-own-base-beside-a-deep-include", func(t *testing.T) {
		outsideRel, err := filepath.Rel(filepath.Join(l.root, "escdeep"), filepath.Join(l.outside, "base.yml"))
		must(t, err)
		dir := l.project(t, "escdeep", map[string]string{
			"compose.yaml":       "include:\n  - x/y/z/compose.yaml\nservices:\n  app:\n    extends:\n      file: " + outsideRel + "\n      service: base\n",
			"x/y/z/compose.yaml": "services:\n  deep:\n    image: busybox\n",
		})
		_, err = l.cb.loadProject(ctx, ProjectEntry{Name: "escdeep", WorkingDir: dir})
		refused(t, err)
	})
	t.Run("chained-extends-escaping", func(t *testing.T) {
		dir := l.project(t, "chainesc", map[string]string{
			"compose.yaml":  "services:\n  app:\n    extends:\n      file: bases/mid.yml\n      service: mid\n",
			"bases/mid.yml": "services:\n  mid:\n    extends:\n      file: " + filepath.Join(l.outside, "base.yml") + "\n      service: base\n",
		})
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "chainesc", WorkingDir: dir})
		refused(t, err)
	})
	// The project's own .env: resolved by resolveLoadPaths, and confined before
	// compose parses it.
	t.Run("top-level-dotenv-symlinked-outside", func(t *testing.T) {
		unparsable := filepath.Join(l.outside, "dotenv-unparsable")
		must(t, os.WriteFile(unparsable, []byte(loadSecret+" !@#\n"), 0o600))
		dir := l.project(t, "dotenvout", map[string]string{"compose.yaml": "services:\n  app:\n    image: alpine\n"})
		must(t, os.Symlink(unparsable, filepath.Join(dir, ".env")))
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "dotenvout", WorkingDir: dir})
		refused(t, err)
	})
	// With the pre-load scan out of the way nothing is approved: the real load's
	// guard must refuse every include and extends reference — even ones inside the
	// root — rather than guess at a base.
	t.Run("real-load-guard-fails-closed-without-the-scan", func(t *testing.T) {
		noScan, err := newComposeBackend(Config{DockerHost: newFakeEngine(t).host(), ComposeRoot: l.root})
		must(t, err)
		noScan.modelCheck = func(context.Context, ProjectEntry, loadPaths, *loadGuard) error { return nil }
		for _, name := range []string{"incabs", "increl", "incok", "extabs", "extok"} {
			_, err := noScan.loadProject(ctx, ProjectEntry{Name: name, WorkingDir: filepath.Join(l.root, name)})
			if err == nil || !strings.Contains(err.Error(), "pre-load scan did not confine") {
				t.Fatalf("%s: want a fail-closed refusal, got %v", name, err)
			}
		}
		// The project's own compose files are approved without the scan.
		if _, err := noScan.loadProject(ctx, ProjectEntry{Name: "overrideok", WorkingDir: filepath.Join(l.root, "overrideok")}); err != nil {
			t.Fatalf("a project with no references must still load: %v", err)
		}
	})
	t.Run("no-compose-file-does-not-walk-up", func(t *testing.T) {
		parent := l.project(t, "parent", map[string]string{"compose.yaml": "services:\n  parentsvc:\n    image: " + loadSecret + "\n"})
		child := filepath.Join(parent, "child")
		must(t, os.MkdirAll(child, 0o755))
		_, err := l.cb.loadProject(ctx, ProjectEntry{Name: "child", WorkingDir: child})
		if err == nil || !strings.Contains(err.Error(), "no compose file") || strings.Contains(err.Error(), loadSecret) {
			t.Fatalf("want no-compose-file without walking to the parent, got %v", err)
		}
	})
	t.Run("compose-file-env-var-is-ignored", func(t *testing.T) {
		dir := l.project(t, "composefile", map[string]string{
			"compose.yaml": "services:\n  app:\n    image: alpine\n",
			".env":         "COMPOSE_FILE=" + filepath.Join(l.outside, "inc.yaml") + "\n",
		})
		p, err := l.cb.loadProject(ctx, ProjectEntry{Name: "composefile", WorkingDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if _, leaked := p.Services["leak"]; leaked {
			t.Fatal("COMPOSE_FILE from .env chose the compose file")
		}
	})
}

// TestIncludeScanRules exercises the include scan on its own, so each rule is
// proven without the load guard or another rule refusing the same input first.
func TestIncludeScanRules(t *testing.T) {
	ctx := context.Background()
	l := newLoadEnv(t)
	scan := projectScan{root: l.root}
	run := func(t *testing.T, name string, files map[string]string) error {
		t.Helper()
		dir := l.project(t, name, files)
		return scan.scan(ctx, []string{filepath.Join(dir, "compose.yaml")}, dir, dir, types.Mapping{}, 0, nil)
	}
	must(t, os.WriteFile(filepath.Join(l.outside, "inc.yaml"), []byte("services:\n  x:\n    image: busybox\n"), 0o644))

	t.Run("include-path-outside-with-project-directory-inside", func(t *testing.T) {
		err := run(t, "rulepath", map[string]string{
			"compose.yaml": "include:\n  - path: " + filepath.Join(l.outside, "inc.yaml") + "\n    project_directory: .\n    env_file: /dev/null\n",
		})
		if err == nil || !strings.Contains(err.Error(), "inc.yaml") || !strings.Contains(err.Error(), "outside docker-agent's compose root") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("project-directory-outside-without-an-env-file", func(t *testing.T) {
		empty := t.TempDir() // outside the root, no .env
		err := run(t, "rulepd", map[string]string{
			"compose.yaml":     "include:\n  - path: lib/compose.yaml\n    project_directory: " + empty + "\n",
			"lib/compose.yaml": "services:\n  x:\n    image: busybox\n",
		})
		if err == nil || !strings.Contains(err.Error(), filepath.Base(empty)) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("string-form", func(t *testing.T) {
		err := run(t, "rulestring", map[string]string{"compose.yaml": "include:\n  - " + filepath.Join(l.outside, "inc.yaml") + "\n"})
		refused(t, err)
	})
	t.Run("cycle", func(t *testing.T) {
		err := run(t, "rulecycle", map[string]string{
			"compose.yaml":   "include:\n  - a/compose.yaml\n",
			"a/compose.yaml": "include:\n  - ../compose.yaml\n",
		})
		if err == nil || !strings.Contains(err.Error(), "include cycle") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("nested-interpolation-sees-the-include-env-file", func(t *testing.T) {
		abs := filepath.Join(l.root, "ruleenv", "a", "vars.env")
		err := run(t, "ruleenv", map[string]string{
			"compose.yaml":   "include:\n  - path: a/compose.yaml\n    env_file: " + abs + "\n",
			"a/vars.env":     "INC=" + filepath.Join(l.outside, "inc.yaml") + "\n",
			"a/compose.yaml": "include:\n  - ${INC}\n",
		})
		refused(t, err)
	})
	t.Run("nested-relative-env-file-uses-compose-working-dir", func(t *testing.T) {
		// compose resolves this env_file against the includer's RELATIVE working dir
		// ("a") — from the process cwd — so that is the file that must be confined.
		t.Chdir(l.outside)
		must(t, os.MkdirAll(filepath.Join(l.outside, "a"), 0o755))
		must(t, os.WriteFile(filepath.Join(l.outside, "a", "quirk.env"), []byte("X=1\n"), 0o644))
		err := run(t, "rulequirk", map[string]string{
			"compose.yaml":     "include:\n  - a/compose.yaml\n",
			"a/compose.yaml":   "include:\n  - path: b/compose.yaml\n    env_file: quirk.env\n",
			"a/b/compose.yaml": "services:\n  x:\n    image: busybox\n",
			"a/quirk.env":      "X=1\n",
		})
		if err == nil || !strings.Contains(err.Error(), filepath.Join(l.outside, "a", "quirk.env")) {
			t.Fatalf("the scan must confine the file compose opens (<cwd>/a/quirk.env): %v", err)
		}
	})
}

func TestLoadGuardApprovedSet(t *testing.T) {
	g := newLoadGuard(nil, []string{"/root/p/compose.yaml"})
	if g.Accept("/root/p/compose.yaml") || g.violation() != nil {
		t.Fatal("a project compose file is approved")
	}
	g.approve("lib/compose.yaml")
	if g.Accept("lib/compose.yaml") || g.violation() != nil {
		t.Fatal("an approved reference falls through to compose's loaders")
	}
	if !g.Accept("other/compose.yaml") {
		t.Fatal("an unapproved reference must be claimed")
	}
	if _, err := g.Load(context.Background(), "other/compose.yaml"); err == nil || g.violation() == nil {
		t.Fatalf("an unapproved reference must be refused: %v", err)
	}
}
