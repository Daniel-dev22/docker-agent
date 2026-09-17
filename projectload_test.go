package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
