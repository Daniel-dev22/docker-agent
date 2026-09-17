package main

// The pre-load scan: every file a compose load will open, found — and confined —
// before any load runs.
//
// compose-go opens files the agent cannot intercept: an include's env_file (to
// interpolate the included files) and project_directory are read without
// consulting any resource loader, and its dotenv parser quotes a line it cannot
// parse. And the files it does open through a resource loader — `include` paths
// and `extends.file` — are resolved against a directory the loader interface never
// passes, so a guard in the loader chain cannot tell which file a relative
// reference means.
//
// So this walks the project the way compose-go v2 does (loader/include.go,
// loader/extends.go, paths/resolve.go) and, for every reference:
//
//   - confines the file it resolves to, from the reference's EXACT base;
//   - records the reference string exactly as compose will hand it to a resource
//     loader, in an approved set. The loader guard (loadGuard, compose.go) lets
//     only approved references through and refuses every other one — it fails
//     CLOSED on anything this walk did not see.
//
// Each file is parsed with compose-go's own loader (interpolation on, include and
// extends processing off), so `${VAR}` resolves as it will in the real load.
//
// The resolution rules mirrored here:
//   - A load context has a loader directory (the local resource loader's): the
//     project's working dir for the project's files; an include's
//     project_directory for the included files.
//   - An include path, and an extends.file, resolve against the loader directory
//     of the file they appear in: absolute as given, relative joined to it.
//   - project_directory: unset → the first path's directory; relative → joined to
//     the includer's working dir; absolute → as given. env_file: unset →
//     <project_directory>/.env when it is a file; relative → joined to the
//     includer's working dir; /dev/null skipped.
//   - The included files load with the includer's environment merged with those
//     env files. Their loader directory is the project_directory; their working dir
//     is that directory made RELATIVE to the includer's loader directory — so a
//     nested include's relative env_file or project_directory resolves against the
//     process's working directory, as compose's os.Stat / os.ReadFile do. Mirrored,
//     not corrected: the scan must confine the file compose actually opens.
//   - An extends.file's base file is read with the same loader directory; the
//     extends.file references inside it are rewritten by compose to
//     <that file's directory relative to the loader directory>/<reference>, and it
//     is those rewritten strings a loader sees when the chain continues. Before
//     rewriting, compose asks every loader whether each RAW reference in the base
//     file is remote (a resource loader's Accept doubles as that predicate), so the
//     raw strings are approved too — as "not remote". Compose never loads a raw
//     base-file reference: the load that follows uses the rewritten one, which is
//     confined from its exact base like every other.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/paths"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
)

// maxScanDepth bounds include nesting and extends chains. Compose has no depth
// limit of its own, only cycle detection; a real stack nests one or two deep.
const maxScanDepth = 16

type projectScan struct {
	root    string
	remotes []loader.ResourceLoader
	// approve receives every local reference string a resource loader will be
	// asked about, once the file it resolves to has been confined.
	approve func(ref string)
}

func (s projectScan) isRemote(ref string) bool {
	for _, r := range s.remotes {
		if r.Accept(ref) {
			return true
		}
	}
	return false
}

// scan confines everything loading configFiles reads. loaderDir resolves
// relative include and extends references; workingDir resolves relative
// project_directory and env_file entries; chain is the include path to here.
func (s projectScan) scan(ctx context.Context, configFiles []string, loaderDir, workingDir string, env types.Mapping, depth int, chain []string) error {
	if depth > maxScanDepth {
		return fmt.Errorf("includes nest deeper than %d levels (%s)", maxScanDepth, echo(strings.Join(chain, " → ")))
	}
	for _, file := range configFiles {
		if err := confinePath(absFromCwd(file), s.root); err != nil {
			return err
		}
		model, err := s.parse(ctx, file, workingDir, env)
		if err != nil {
			return err
		}
		includes, err := includeEntries(model, file)
		if err != nil {
			return err
		}
		for _, inc := range includes {
			if err := s.scanInclude(ctx, inc, loaderDir, workingDir, env, depth, append(chain, file)); err != nil {
				return err
			}
		}
		services, _ := model["services"].(map[string]any)
		for name := range services {
			if err := s.scanExtends(ctx, services, name, file, loaderDir, workingDir, env, 0, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s projectScan) scanInclude(ctx context.Context, inc types.IncludeConfig, loaderDir, workingDir string, env types.Mapping, depth int, chain []string) error {
	var files, refs []string
	for _, p := range inc.Path {
		if s.isRemote(p) {
			continue // remote content, fetched by compose's git/OCI loaders
		}
		resolved := p
		if !filepath.IsAbs(p) {
			resolved = filepath.Join(loaderDir, p)
		}
		for _, seen := range chain {
			if seen == resolved {
				return fmt.Errorf("include cycle: %s", echo(strings.Join(append(chain, resolved), " → ")))
			}
		}
		// Confined by the recursive scan below, before the file is read.
		files = append(files, resolved)
		refs = append(refs, p)
	}
	if len(files) == 0 {
		return nil
	}

	projectDir, relWorkingDir := inc.ProjectDirectory, ""
	switch {
	case projectDir == "":
		relWorkingDir = loaderRel(loaderDir, files[0])
		projectDir = filepath.Dir(files[0])
	case !filepath.IsAbs(projectDir):
		relWorkingDir = loaderRel(loaderDir, projectDir)
		projectDir = filepath.Join(workingDir, projectDir)
	default:
		relWorkingDir = projectDir
	}
	if err := confinePath(absFromCwd(projectDir), s.root); err != nil {
		return err
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
	if err := s.scan(ctx, files, projectDir, relWorkingDir, env.Clone().Merge(fromFiles), depth+1, chain); err != nil {
		return err
	}
	for _, ref := range refs {
		s.approveRef(ref)
	}
	return nil
}

// approveRef approves a reference. Called only after everything it resolves to
// has been confined.
func (s projectScan) approveRef(ref string) {
	if s.approve != nil {
		s.approve(ref)
	}
}

// extendsRef identifies one step of an extends chain, for cycle detection.
type extendsRef struct{ file, service string }

// scanExtends follows services[name]'s extends chain the way compose-go does.
// file is the compose file services came from; loaderDir is the load context's
// loader directory, which every file reference in the chain resolves against.
func (s projectScan) scanExtends(ctx context.Context, services map[string]any, name, file, loaderDir, workingDir string,
	env types.Mapping, depth int, seen []extendsRef) error {
	svc, _ := services[name].(map[string]any)
	if svc == nil {
		return nil
	}
	var ref, ext string
	hasFile := false
	switch v := svc["extends"].(type) {
	case string:
		ref = v
	case map[string]any:
		ref, _ = v["service"].(string)
		if f, ok := v["file"]; ok && f != nil {
			ext, _ = f.(string)
			hasFile = true
		}
	default:
		return nil
	}
	if depth > maxScanDepth {
		return fmt.Errorf("extends chain deeper than %d levels at %s", maxScanDepth, echo(name))
	}
	step := extendsRef{file: file, service: name}
	for _, prev := range seen {
		if prev == step {
			return fmt.Errorf("extends cycle at service %s in %s", echo(name), echo(file))
		}
	}
	seen = append(seen, step)

	if !hasFile {
		return s.scanExtends(ctx, services, ref, file, loaderDir, workingDir, env, depth+1, seen)
	}
	if s.isRemote(ext) {
		return nil
	}
	resolved := ext
	if !filepath.IsAbs(ext) {
		resolved = filepath.Join(loaderDir, ext)
	}
	if err := confinePath(absFromCwd(resolved), s.root); err != nil {
		return err
	}
	base, err := s.parse(ctx, resolved, workingDir, env)
	if err != nil {
		return err
	}
	baseServices, _ := base["services"].(map[string]any)
	// compose rewrites the base file's own extends.file references relative to
	// that file's directory, as seen from the loader directory — after asking each
	// loader whether the raw reference is remote.
	relDir := loaderRel(loaderDir, ext)
	for _, raw := range baseServices {
		bs, _ := raw.(map[string]any)
		bext, _ := bs["extends"].(map[string]any)
		f, ok := bext["file"].(string)
		if !ok || s.isRemote(f) {
			continue
		}
		s.approveRef(f) // the remote predicate's question, not a load
		f = paths.ExpandUser(f)
		if !filepath.IsAbs(f) && f != "" {
			f = filepath.Join(relDir, f)
		}
		bext["file"] = f
	}
	if err := s.scanExtends(ctx, baseServices, ref, resolved, loaderDir, workingDir, env, depth+1, seen); err != nil {
		return err
	}
	s.approveRef(ext)
	return nil
}

// parse reads one compose file into its interpolated model without processing
// includes or extends.
func (s projectScan) parse(ctx context.Context, file, workingDir string, env types.Mapping) (map[string]any, error) {
	return loader.LoadModelWithContext(ctx, types.ConfigDetails{
		WorkingDir:  workingDir,
		ConfigFiles: []types.ConfigFile{{Filename: absFromCwd(file)}},
		Environment: env,
	}, func(o *loader.Options) {
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipValidation = true
		o.SkipNormalization = true
		o.SkipConsistencyCheck = true
		o.SkipDefaultValues = true
		o.ResolvePaths = false
		o.SetProjectName("project-scan", true)
		quietInterpolation(o)
	})
}

// quietInterpolation interpolates exactly as compose does, without compose-go's
// "variable is not set" warning: a pre-load pass reads the same variables as the
// real load, which reports a genuinely unset one — once, not once per pass.
func quietInterpolation(o *loader.Options) {
	if o.Interpolate != nil {
		o.Interpolate.Substitute = func(s string, m template.Mapping) (string, error) {
			return template.SubstituteWithOptions(s, m, template.WithoutLogging)
		}
	}
}

// includeEntries decodes a model's top-level include list. compose-go's
// Transform decodes the short string form itself.
func includeEntries(model map[string]any, file string) ([]types.IncludeConfig, error) {
	raw, ok := model["include"].([]any)
	if !ok {
		return nil, nil
	}
	var includes []types.IncludeConfig
	if err := loader.Transform(raw, &includes); err != nil {
		return nil, fmt.Errorf("parse include in %s: %w", echo(file), err)
	}
	return includes, nil
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

// absFromCwd resolves a path the way the kernel does for a relative open.
func absFromCwd(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
