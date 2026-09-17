package main

// Confining what `include:` reads before compose processes it.
//
// compose-go's include processing opens an include's env_file (to interpolate the
// included files) and stats files under its project_directory without consulting
// any resource loader or listener, and its dotenv parser quotes a line it cannot
// parse. So before either compose load runs, this walks every compose file's
// `include:` entries — string, list, and long syntax with path / env_file /
// project_directory — the way compose-go's ApplyInclude resolves them, confines
// every local path it would touch, and recurses into the included files with the
// environment and directories compose would use. The walk parses each file with
// compose-go's own loader (interpolation on, include/extends processing off), so
// `${VAR}` in an include resolves exactly as it will in the real load.
//
// The resolution rules mirrored here (compose-go v2 loader/include.go):
//   - An include path is resolved by the includer's local loader: absolute as
//     given, relative against that loader's directory (the project's working dir,
//     or the including include's project_directory).
//   - project_directory: unset → the first path's directory; relative → joined to
//     the includer's working dir; absolute → as given.
//   - env_file: unset → <project_directory>/.env when it is a file; each relative
//     entry → joined to the includer's working dir; /dev/null is skipped.
//   - The included files load with the includer's environment merged with those
//     env files. Their loader directory is the new project_directory, but their
//     working dir is that directory made RELATIVE to the includer's loader
//     directory — so a nested include's relative env_file or project_directory
//     resolves against the process's working directory, as compose's os.Stat /
//     os.ReadFile do. That is compose's behaviour, mirrored rather than corrected:
//     the scan must confine the file compose will actually open.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// maxIncludeDepth bounds the walk. Compose has no depth limit of its own, only
// cycle detection; a real stack nests includes one or two deep.
const maxIncludeDepth = 16

type includeScan struct {
	root    string
	remotes []loader.ResourceLoader
	// onProjectDir receives every included project's directory, absolute: the
	// directories compose's loader resolves that project's relative references
	// against (see loadGuard).
	onProjectDir func(dir string)
}

func (s includeScan) isRemote(ref string) bool {
	for _, r := range s.remotes {
		if r.Accept(ref) {
			return true
		}
	}
	return false
}

// scan confines everything the include processing of configFiles would read.
// loaderDir resolves relative include paths; workingDir resolves relative
// project_directory and env_file entries; chain is the include path to here.
func (s includeScan) scan(ctx context.Context, configFiles []string, loaderDir, workingDir string, env types.Mapping, depth int, chain []string) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("includes nest deeper than %d levels (%s)", maxIncludeDepth, echo(strings.Join(chain, " → ")))
	}
	for _, file := range configFiles {
		if err := confinePath(file, s.root); err != nil {
			return err
		}
		includes, err := s.includesOf(ctx, file, workingDir, env)
		if err != nil {
			return err
		}
		for _, inc := range includes {
			if err := s.scanInclude(ctx, inc, loaderDir, workingDir, env, depth, append(chain, file)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s includeScan) scanInclude(ctx context.Context, inc types.IncludeConfig, loaderDir, workingDir string, env types.Mapping, depth int, chain []string) error {
	var paths []string
	for _, p := range inc.Path {
		if s.isRemote(p) {
			continue // remote content, fetched by compose's git/OCI loaders
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(loaderDir, p)
		}
		for _, seen := range chain {
			if seen == p {
				return fmt.Errorf("include cycle: %s", echo(strings.Join(append(chain, p), " → ")))
			}
		}
		if err := confinePath(p, s.root); err != nil {
			return err
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return nil
	}

	projectDir, relWorkingDir := inc.ProjectDirectory, ""
	switch {
	case projectDir == "":
		relWorkingDir = loaderRel(loaderDir, paths[0])
		projectDir = filepath.Dir(paths[0])
	case !filepath.IsAbs(projectDir):
		relWorkingDir = loaderRel(loaderDir, projectDir)
		projectDir = filepath.Join(workingDir, projectDir)
	default:
		relWorkingDir = projectDir
	}
	if err := confinePath(absFromCwd(projectDir), s.root); err != nil {
		return err
	}
	if s.onProjectDir != nil {
		s.onProjectDir(absFromCwd(projectDir))
	}

	var envFiles []string
	if len(inc.EnvFile) == 0 {
		dotenvPath := filepath.Join(projectDir, ".env")
		if readableFile(absFromCwd(dotenvPath)) {
			envFiles = append(envFiles, dotenvPath)
		}
	}
	for _, f := range inc.EnvFile {
		if f == "/dev/null" {
			continue
		}
		if !filepath.IsAbs(f) {
			f = filepath.Join(workingDir, f)
		}
		envFiles = append(envFiles, f)
	}
	for _, f := range envFiles {
		if err := confinePath(absFromCwd(f), s.root); err != nil {
			return err
		}
	}
	fromFiles, err := dotenv.GetEnvFromFile(env, envFiles)
	if err != nil {
		return err
	}
	return s.scan(ctx, paths, projectDir, relWorkingDir, env.Clone().Merge(fromFiles), depth+1, chain)
}

// loaderRel is compose-go's localResourceLoader.Dir: the directory of a reference
// (the reference itself when it is a directory), relative to the loader's
// directory when it can be made so.
func loaderRel(loaderDir, ref string) string {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(loaderDir, p)
	}
	path := abs(ref)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		path = abs(filepath.Dir(ref))
	}
	rel, err := filepath.Rel(loaderDir, path)
	if err != nil {
		return path
	}
	return rel
}

// includesOf parses file (interpolated with env, relative to workingDir) and
// returns its top-level include entries, without processing them.
func (s includeScan) includesOf(ctx context.Context, file, workingDir string, env types.Mapping) ([]types.IncludeConfig, error) {
	model, err := loader.LoadModelWithContext(ctx, types.ConfigDetails{
		WorkingDir:  workingDir,
		ConfigFiles: []types.ConfigFile{{Filename: file}},
		Environment: env,
	}, func(o *loader.Options) {
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipValidation = true
		o.SkipNormalization = true
		o.SkipConsistencyCheck = true
		o.SkipDefaultValues = true
		o.ResolvePaths = false
		o.SetProjectName("include-scan", true)
	})
	if err != nil {
		return nil, err
	}
	raw, ok := model["include"].([]any)
	if !ok {
		return nil, nil
	}
	for i, entry := range raw {
		if p, ok := entry.(string); ok {
			raw[i] = map[string]any{"path": p}
		}
	}
	var includes []types.IncludeConfig
	if err := loader.Transform(raw, &includes); err != nil {
		return nil, fmt.Errorf("parse include in %s: %w", echo(file), err)
	}
	return includes, nil
}

// absFromCwd resolves a path the way the kernel does for a relative open.
func absFromCwd(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
