package main

// Which files a project reads, and where they may live.
//
// A registry entry names its files: compose_files and env_files, absolute or
// relative to the working dir. Nothing structural stops such a path from pointing
// anywhere the agent's container can read — an env file naming
// /etc/docker-agent/bearer-token, a docker-compose.yml under ComposeRoot that is a
// symlink out of it, an `include:` or `extends.file` in the compose model, a
// service `env_file:` — and a compose load echoes parse errors into the job log,
// and the bundle endpoint returns file CONTENT, over an API reachable in-network
// with no bearer. So:
//
//   - resolveLoadPaths is THE resolver: it absolutises compose and env paths
//     against the working dir, and its output is exactly what compose is given.
//     Compose is always handed explicit config paths, so its own discovery —
//     COMPOSE_FILE, default names, the override file, the walk up parent
//     directories — never runs.
//   - Register rejects declared paths escaping the working dir.
//   - Every read — bundle, copy, compose load — confines each file, symlinks
//     followed, under ComposeRoot; the load also confines what compose reaches
//     from inside the model (loadGuard, compose.go).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
)

// loadPaths is what a project load reads before compose parses anything.
type loadPaths struct {
	config []string // compose files, in load order
	env    []string // project env files (interpolation)
}

func (p loadPaths) all() []string { return append(append([]string{}, p.config...), p.env...) }

// absAgainst makes f absolute against the working dir.
func absAgainst(workingDir, f string) string {
	if filepath.IsAbs(f) || workingDir == "" {
		return filepath.Clean(f)
	}
	return filepath.Join(workingDir, f)
}

// resolveLoadPaths resolves the files compose will load for e, exactly as they
// will be passed to it.
//
//   - Declared compose files, absolutised; with none declared, the first of
//     compose's own default names present in the working dir, plus the first
//     override file there — the pair compose's discovery would pick, minus its
//     walk up the parent directories.
//   - Declared env files, absolutised (compose would resolve a relative one
//     against the process cwd, "/"); with none declared, the working dir's .env
//     when it is a file — compose's own default.
func resolveLoadPaths(e ProjectEntry) (loadPaths, error) {
	var p loadPaths
	for _, f := range e.ComposeFiles {
		p.config = append(p.config, absAgainst(e.WorkingDir, f))
	}
	if len(p.config) == 0 {
		if main := firstPresent(e.WorkingDir, cli.DefaultFileNames); main != "" {
			p.config = append(p.config, main)
			if override := firstPresent(e.WorkingDir, cli.DefaultOverrideFileNames); override != "" {
				p.config = append(p.config, override)
			}
		}
	}
	if len(p.config) == 0 {
		return p, fmt.Errorf("no compose file (%s) in %s", strings.Join(cli.DefaultFileNames, ", "), echo(e.WorkingDir))
	}
	for _, f := range e.EnvFiles {
		p.env = append(p.env, absAgainst(e.WorkingDir, f))
	}
	if len(e.EnvFiles) == 0 && e.WorkingDir != "" {
		dotenv := filepath.Join(e.WorkingDir, ".env")
		if st, err := os.Stat(dotenv); err == nil && !st.IsDir() {
			p.env = append(p.env, dotenv)
		}
	}
	return p, nil
}

func firstPresent(dir string, names []string) string {
	if dir == "" {
		return ""
	}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Lstat(p); err == nil {
			return p
		}
	}
	return ""
}

// composeFilesPresent: a path-only register under ComposeRoot claims managed:true,
// so every compose file the load would read must be a readable regular file.
func composeFilesPresent(e ProjectEntry) error {
	paths, err := resolveLoadPaths(e)
	if err != nil {
		return err
	}
	for _, f := range paths.config {
		if !readableFile(f) {
			return fmt.Errorf("compose file %s for %s is missing or unreadable", echo(f), echo(e.Name))
		}
	}
	return nil
}

func readableFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	return err == nil && st.Mode().IsRegular()
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
	for _, f := range append(append([]string{}, e.ComposeFiles...), e.EnvFiles...) {
		if len(f) > maxPathLen {
			return fmt.Errorf("a declared path is longer than %d bytes", maxPathLen)
		}
		p := absAgainst(wd, f)
		if !within(p, wd) {
			return fmt.Errorf("%s is outside the project's working dir %s", echo(p), echo(wd))
		}
		if wdErr != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue // not visible here
		}
		if !within(real, realWD) {
			return fmt.Errorf("%s resolves to %s, outside the project's working dir %s", echo(p), echo(real), echo(wd))
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

// errOutsideRoot is the refusal every confinement check returns. Its text names
// the path, never content.
type errOutsideRoot struct{ path, root string }

func (e errOutsideRoot) Error() string {
	return fmt.Sprintf("%s resolves outside docker-agent's compose root (%s); refusing to read it", echo(e.path), e.root)
}

// confinePath refuses a file that resolves outside root, or whose lexical path
// already does when it does not exist. A missing file under root is left to
// compose to report.
func confinePath(p, root string) error {
	ok, err := confinedUnder(p, root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		realRoot, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			return fmt.Errorf("resolve compose root: %w", rerr)
		}
		if !within(filepath.Clean(p), root) && !within(filepath.Clean(p), realRoot) {
			return errOutsideRoot{p, root}
		}
		return nil
	case err != nil:
		return fmt.Errorf("check %s: %w", echo(p), err)
	case !ok:
		return errOutsideRoot{p, root}
	}
	return nil
}
