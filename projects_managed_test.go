package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUnderComposeRoot covers the structural ownership predicate: a stack whose
// working dir is under ComposeRoot is agent-owned/editable; anything else (incl.
// a sibling that merely shares a path prefix) is not.
func TestUnderComposeRoot(t *testing.T) {
	root := "/srv/containers/docker-agent/data"
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"created-under-root", root + "/mystack", true},
		{"root-itself", root, true},
		{"sibling-dir", "/srv/containers/other-agent", false},
		{"prefix-not-subdir", root + "-evil/stack", false},
		{"unrelated", "/opt/stacks/compose/42", false},
		{"empty-dir", "", false},
		{"empty-root", "", true}, // sentinel handled below
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := root
			dir := c.dir
			want := c.want
			if c.name == "empty-root" { // exercise the empty-root guard
				r, dir, want = "", root, false
			}
			if got := underComposeRoot(dir, r); got != want {
				t.Fatalf("underComposeRoot(%q, %q) = %v, want %v", dir, r, got, want)
			}
		})
	}
}

// TestMergeKnownManaged verifies mergeKnown flags Managed only for registry
// entries the agent owns (under ComposeRoot), regardless of whether their
// containers are currently live.
func TestMergeKnownManaged(t *testing.T) {
	root := t.TempDir()
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)

	owned := ProjectEntry{Name: "owned", WorkingDir: filepath.Join(root, "owned")}
	external := ProjectEntry{Name: "external", WorkingDir: "/mnt/elsewhere/other-stack"}
	if err := reg.register(owned); err != nil {
		t.Fatal(err)
	}
	if err := reg.register(external); err != nil {
		t.Fatal(err)
	}

	// "owned" is also live (running); "external" is stopped (registry-only).
	live := []ComposeProject{{Name: "owned", WorkingDir: owned.WorkingDir, RunningCount: 1}}
	out := reg.mergeKnown(live, nil)

	got := map[string]bool{}
	for _, p := range out {
		got[p.Name] = p.Managed
	}
	if !got["owned"] {
		t.Errorf("owned stack (under ComposeRoot) should be Managed=true")
	}
	if got["external"] {
		t.Errorf("external stack (outside ComposeRoot) should be Managed=false")
	}
}

// TestReadBundleGraceful verifies readBundle returns (bundle, nil) — not a hard
// error — when a compose file is unreadable (the externally-managed case), with
// empty ComposeFiles + working_dir so the UI degrades gracefully; and returns
// the content for a readable file.
func TestReadBundleGraceful(t *testing.T) {
	a := &app{}

	t.Run("unreadable-skips-no-error", func(t *testing.T) {
		e := ProjectEntry{
			Name:         "external",
			WorkingDir:   "/mnt/elsewhere/other-stack",
			ComposeFiles: []string{"/mnt/elsewhere/other-stack/docker-compose.yml"},
		}
		b, err := a.readBundle(e)
		if err != nil {
			t.Fatalf("expected nil error for unreadable file, got %v", err)
		}
		if len(b.ComposeFiles) != 0 {
			t.Errorf("expected empty ComposeFiles, got %d", len(b.ComposeFiles))
		}
		if b.WorkingDir != e.WorkingDir {
			t.Errorf("working_dir not preserved: got %q", b.WorkingDir)
		}
	})

	t.Run("readable-returns-content", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "owned")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "services:\n  app:\n    image: alpine\n"
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		e := ProjectEntry{Name: "owned", WorkingDir: dir, ComposeFiles: []string{"docker-compose.yml"}}
		b, err := (&app{cfg: Config{ComposeRoot: root}}).readBundle(e)
		if err != nil {
			t.Fatal(err)
		}
		if len(b.ComposeFiles) != 1 || b.ComposeFiles[0].Content != body {
			t.Fatalf("expected the compose content back, got %+v", b.ComposeFiles)
		}
	})
}
