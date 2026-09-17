package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkValidProjectName is the per-project cost the capability check pays on
// every snapshot row.
func BenchmarkValidProjectName(b *testing.B) {
	names := []string{"homeassistant", "duplicacy-agent-api", "Docker-Agent", "my.stack"}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		_ = validProjectName(names[i%len(names)])
	}
}

// BenchmarkMergeKnown100 is one fleet-snapshot capability pass over 100 projects
// (50 live, 50 registry-only) — well above the largest live host.
func BenchmarkMergeKnown100(b *testing.B) {
	root := b.TempDir()
	reg := newComposeRegistry(filepath.Join(root, "projects.json"), root)
	live := make([]ComposeProject, 0, 50)
	for i := range 100 {
		name := fmt.Sprintf("stack-%03d", i)
		reg.byName[name] = &ProjectEntry{Name: name, WorkingDir: filepath.Join(root, name)}
		if i < 50 {
			live = append(live, ComposeProject{Name: name, WorkingDir: filepath.Join(root, name), RunningCount: 1})
		}
	}
	v := deriveSelfView(testSelfID, "traefik", nil, time.Time{})
	b.ReportAllocs()
	for b.Loop() {
		in := append([]ComposeProject(nil), live...)
		_ = reg.mergeKnown(in, v)
	}
}
