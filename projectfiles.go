package main

// Where a project's compose and env files may live.
//
// A registry entry names its files: compose_files and env_files, absolute or
// relative to the working dir. Nothing structural stops such a path from pointing
// anywhere the agent's container can read — a path-only register naming
// /etc/docker-agent/bearer-token as an env file, or a docker-compose.yml under
// ComposeRoot that is a symlink out of it — and the bundle endpoint returns file
// CONTENT, over an API reachable in-network with no bearer. So two rules:
//
//   - At register time, every declared path must stay inside the entry's working
//     dir (lexically, and after resolving symlinks when the file is visible).
//   - Whenever the agent READS project files (bundle, copy, loading the compose
//     model), every file must resolve — symlinks followed — under ComposeRoot.
//     A file that does not is skipped by the bundle and refuses the load.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// declaredProjectPaths returns the entry's declared compose and env file paths,
// each made absolute against the working dir.
func declaredProjectPaths(e ProjectEntry) []string {
	out := make([]string, 0, len(e.ComposeFiles)+len(e.EnvFiles))
	for _, f := range append(append([]string{}, e.ComposeFiles...), e.EnvFiles...) {
		if !filepath.IsAbs(f) && e.WorkingDir != "" {
			f = filepath.Join(e.WorkingDir, f)
		}
		out = append(out, filepath.Clean(f))
	}
	return out
}

// within reports whether path is dir or inside it (lexical; both cleaned).
func within(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// validateDeclaredPaths is the register-time rule: every declared compose/env path
// stays inside the working dir. A file the agent can see is also checked after
// resolving symlinks; one it cannot see (an Ansible-owned stack outside the mount)
// is checked lexically, which is all that can be known — and all that matters,
// since the agent never reads it.
func validateDeclaredPaths(e ProjectEntry) error {
	if e.WorkingDir == "" {
		return nil
	}
	wd := filepath.Clean(e.WorkingDir)
	realWD, wdErr := filepath.EvalSymlinks(wd)
	for _, p := range declaredProjectPaths(e) {
		if !within(p, wd) {
			return fmt.Errorf("%s is outside the project's working dir %s", p, wd)
		}
		if wdErr != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue // not visible here
		}
		if !within(real, realWD) {
			return fmt.Errorf("%s resolves to %s, outside the project's working dir %s", p, real, wd)
		}
	}
	return nil
}

// confinedUnder reports whether path, with symlinks resolved, is under root (also
// resolved). A path that does not exist returns fs.ErrNotExist.
func confinedUnder(path, root string) (bool, error) {
	if root == "" {
		return false, errors.New("compose root not configured")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, fmt.Errorf("resolve compose root: %w", err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	return within(real, realRoot), nil
}

// composeFilesToLoad is what loading the project reads: the declared compose
// files, or — with none declared — whichever of compose's default names exist in
// the working dir.
func composeFilesToLoad(e ProjectEntry) []string {
	if files := e.absComposeFiles(); len(files) > 0 {
		return files
	}
	var out []string
	for _, name := range composeDefaultFiles {
		p := filepath.Join(e.WorkingDir, name)
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// envFilesToLoad is every env file loading reads: the declared ones plus the
// working dir's .env.
func envFilesToLoad(e ProjectEntry) []string {
	var out []string
	for _, ef := range e.EnvFiles {
		if !filepath.IsAbs(ef) && e.WorkingDir != "" {
			ef = filepath.Join(e.WorkingDir, ef)
		}
		out = append(out, ef)
	}
	if e.WorkingDir != "" {
		out = append(out, filepath.Join(e.WorkingDir, ".env"))
	}
	return out
}

// confineProjectFiles refuses a load whose compose or env files resolve outside
// root. A file that does not exist is left to compose to report.
func confineProjectFiles(e ProjectEntry, root string) error {
	for _, p := range append(composeFilesToLoad(e), envFilesToLoad(e)...) {
		ok, err := confinedUnder(p, root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check %s: %w", p, err)
		}
		if !ok {
			return fmt.Errorf("%s resolves outside docker-agent's compose root (%s); refusing to read it", p, root)
		}
	}
	return nil
}
