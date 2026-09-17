package main

// On-disk image changes for the stack-update engine.
//
// When a resolver moves a service to a new image, the change is written into the
// project's own files so the next `docker compose up` — the agent's, an
// operator's, Ansible's — deploys the same thing. Those files belong to whoever
// wrote them, so a write keeps four guarantees and refuses rather than weaken one:
//
//  1. It changes one token per service. A literal image is rewritten where it is
//     spelled: in the last compose file, and the last document in that file, that
//     sets it (compose merges files and documents in order). A `${VAR}` image is
//     rewritten as the value of VAR's assignment in the env file compose takes it
//     from; a template whose only variable part is a trailing tag (`reg/app:${TAG}`)
//     as the value of that tag variable. Every other byte — formatting, comments,
//     quotes, line endings, other documents — is left alone. Whatever cannot be
//     changed that way is refused with no file touched: an anchored or aliased
//     image, a tagged, block or escaped scalar, a merge key, a template that varies
//     anything but a trailing tag, or a spelling that would read back differently.
//  2. It is verified by the loader that deploys it. After writing, the project is
//     loaded exactly as the agent loads it. Every target must resolve to its new
//     image, and nothing else may differ: not another service that shares the
//     variable, not an assignment shadowed by a later line or by the agent's own
//     environment. Otherwise the original bytes go back and the write fails.
//  3. A revert restores BYTES, never values: the exact content each file had, or no
//     file where there was none. A per-service revert rebuilds each file from those
//     bytes with only the kept services' edits applied — never by running the
//     writer again with an old value, which reformatted files and, restoring
//     per variable in map order, left an empty .env behind.
//  4. It runs under the project's lock (projectlock.go): two jobs never write one
//     project's files at once.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	yaml "go.yaml.in/yaml/v4"
)

// projectLoader loads a project the way the agent deploys it (composeBackend.loadProject).
type projectLoader func(context.Context, ProjectEntry) (*types.Project, error)

// textEdit replaces [start, end) of a file's original bytes with text.
type textEdit struct {
	start, end int
	text       string
	owners     []string // the target services that need this edit
	// imageCheck, for a compose-file edit: the node that must read back as the
	// new image once the edit is applied.
	imageCheck *yamlImagePath
}

// yamlImagePath addresses services.<service>.image in one document of a file.
type yamlImagePath struct {
	doc     int
	service string
	image   string
	style   yaml.Style
}

// fileState is one file as the write found it, and what the write made of it.
type fileState struct {
	path     string
	original []byte // exact bytes before the write (nil when !existed)
	existed  bool
	// mode applies only if the write creates the file; an existing file keeps
	// its own (writeFileAtomic).
	mode    os.FileMode
	edits   []*textEdit  // every edit the write made, whichever services later revert
	written []byte       // the bytes the write put there: original with every edit
	docs    []*yaml.Node // parsed documents, compose files only
	// present and current are what the agent last left in the file: the write's
	// bytes, then each revert's. A revert compares the disk against them to tell a
	// hand edit from its own work — never against written, which a first partial
	// revert has already replaced.
	present bool
	current []byte
}

// imageWrite is a verified on-disk image change and everything needed to undo it.
type imageWrite struct {
	entry    ProjectEntry
	targets  map[string]string // service → image actually changed
	baseline *types.Project    // the project before the write
	project  *types.Project    // the project as loaded after the write
	files    []*fileState      // the files written, in write order
	// reverted accumulates across partial reverts: a later revert rebuilds each
	// file with only the edits of services no revert has named yet, so reverting
	// [app] and then [net] ends where reverting both at once does — the original.
	reverted map[string]bool
}

// writeServiceImages points each service in targets at its image on disk, verifies
// the result with load, and returns what it changed. On any error no file differs
// from what it was.
func writeServiceImages(ctx context.Context, entry ProjectEntry, targets map[string]string, load projectLoader) (*imageWrite, error) {
	for _, svc := range slices.Sorted(maps.Keys(targets)) {
		if img := targets[svc]; !validImageRef(img) {
			if isImageID(img) {
				return nil, fmt.Errorf("refusing to write %s: %w", svc, imageIDError(svc, img))
			}
			return nil, fmt.Errorf("refusing to write %q as %s's image: not an image reference", echo(img), svc)
		}
	}
	baseline, err := load(ctx, entry)
	if err != nil {
		return nil, fmt.Errorf("load project before writing: %w", err)
	}
	paths, err := resolveLoadPaths(entry)
	if err != nil {
		return nil, err
	}
	w := &imageWrite{entry: entry, targets: map[string]string{}, baseline: baseline, project: baseline, reverted: map[string]bool{}}
	plan := &writePlan{entry: entry, paths: paths, files: map[string]*fileState{}, vars: map[string]*varEdit{}}
	for _, svc := range slices.Sorted(maps.Keys(targets)) {
		img := targets[svc]
		cur, ok := baseline.Services[svc]
		if !ok {
			return nil, fmt.Errorf("service %s is not in project %s", svc, entry.Name)
		}
		if cur.Image == img {
			continue
		}
		w.targets[svc] = img
		if err := plan.planImage(svc, img, cur.Image); err != nil {
			return nil, fmt.Errorf("%s: %w", svc, err)
		}
	}
	if len(w.targets) == 0 {
		return w, nil
	}
	if w.files, err = plan.render(); err != nil {
		return nil, err
	}
	if err := w.writeAll(); err != nil {
		return nil, err
	}
	after, err := load(ctx, entry)
	if err == nil {
		err = verifyImages(baseline, after, w.targets)
	} else {
		err = fmt.Errorf("the project no longer loads: %w", err)
	}
	if err != nil {
		if rerr := w.restoreAll(); rerr != nil {
			return nil, fmt.Errorf("%w — and restoring the original files failed: %v", err, rerr)
		}
		return nil, fmt.Errorf("%w; the original files are restored", err)
	}
	w.project = after
	return w, nil
}

// described lists the files a write changed, for the job log.
func (w *imageWrite) described() []string {
	var out []string
	for _, f := range w.files {
		out = append(out, f.path)
	}
	return out
}

// ---------------------------------------------------------------------------
// planning
// ---------------------------------------------------------------------------

type writePlan struct {
	entry ProjectEntry
	paths loadPaths
	files map[string]*fileState
	order []string
	vars  map[string]*varEdit
}

type varEdit struct {
	value string
	edit  *textEdit
	file  *fileState
	// inserted: the write declares the variable (no statement did); use, when an
	// env-file template references it, is the earliest such statement — the
	// declaration must sit above it.
	inserted bool
	use      *envUse
}

// envUse is where an env-file template references a variable: dotenv expands a
// value when it reads it, so a declaration takes effect only above that point.
type envUse struct {
	file   *fileState
	order  int // the file's position in the project's env files
	offset int // the start of the referencing statement's line
}

func (u *envUse) before(o *envUse) bool {
	return u.order < o.order || (u.order == o.order && u.offset < o.offset)
}

func (p *writePlan) file(path string) (*fileState, error) {
	if f, ok := p.files[path]; ok {
		return f, nil
	}
	f := &fileState{path: path, mode: 0o600}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		f.original, f.existed = data, true
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	p.files[path] = f
	p.order = append(p.order, path)
	return f, nil
}

func (f *fileState) yamlDocs() ([]*yaml.Node, error) {
	if f.docs != nil || !f.existed {
		return f.docs, nil
	}
	docs, err := decodeYAMLDocs(f.original)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	f.docs = docs
	return docs, nil
}

func decodeYAMLDocs(data []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	for {
		n := &yaml.Node{}
		err := dec.Decode(n)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, n)
	}
}

func (p *writePlan) planImage(svc, img, current string) error {
	loc, err := p.locateImage(svc)
	if err != nil {
		return err
	}
	raw := loc.node.Value
	if !strings.Contains(raw, "$") {
		return p.planLiteral(loc, svc, img)
	}
	if name, ok := wholeVarRef(raw); ok {
		return p.planVar(name, img, svc, current, 0, nil)
	}
	if name, ok := trailingTagVar(raw); ok {
		tag, err := tagChange(current, img)
		if err != nil {
			return fmt.Errorf("image %q varies only its tag (${%s}): %w", raw, name, err)
		}
		return p.planVar(name, tag, svc, "", 0, nil)
	}
	return fmt.Errorf("image %q in %s interpolates more than a whole-value variable or a single trailing tag "+
		"variable; the agent will not guess which variable to change", raw, loc.file.path)
}

type imageLoc struct {
	file *fileState
	doc  int
	node *yaml.Node
}

// locateImage finds where compose takes service's image from: the last compose
// file, and the last document within it, whose service mapping sets `image`.
func (p *writePlan) locateImage(svc string) (imageLoc, error) {
	for i := len(p.paths.config) - 1; i >= 0; i-- {
		f, err := p.file(p.paths.config[i])
		if err != nil {
			return imageLoc{}, err
		}
		docs, err := f.yamlDocs()
		if err != nil {
			return imageLoc{}, err
		}
		for d := len(docs) - 1; d >= 0; d-- {
			node, err := serviceImageNode(docs[d], svc)
			if err != nil {
				return imageLoc{}, fmt.Errorf("%s: %w", f.path, err)
			}
			if node == nil {
				continue
			}
			if err := inPlaceImage(node); err != nil {
				return imageLoc{}, fmt.Errorf("%s: %w", f.path, err)
			}
			return imageLoc{file: f, doc: d, node: node}, nil
		}
	}
	return imageLoc{}, fmt.Errorf("no compose file sets its image directly (it may come from extends, an include, or a merge key)")
}

// serviceImageNode returns the image node services.<svc>.image in one document,
// nil when that document does not set it, and an error for a shape the image
// cannot be located in without guessing.
func serviceImageNode(doc *yaml.Node, svc string) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	services := mapValue(doc.Content[0], "services")
	if services == nil || services.ShortTag() == "!!null" {
		return nil, nil
	}
	if services.Kind != yaml.MappingNode || services.Tag != "!!map" {
		return nil, fmt.Errorf("services is not a plain mapping")
	}
	node := mapValue(services, svc)
	switch {
	case node == nil || node.ShortTag() == "!!null":
		return nil, nil
	case node.Kind == yaml.AliasNode:
		return nil, fmt.Errorf("service %s is a YAML alias", svc)
	case node.Kind != yaml.MappingNode || node.Tag != "!!map":
		return nil, fmt.Errorf("service %s is not a plain mapping (tag %s)", svc, node.Tag)
	}
	image := mapValue(node, "image")
	if image == nil && mapValue(node, "<<") != nil {
		return nil, fmt.Errorf("service %s takes its keys from a YAML merge key", svc)
	}
	return image, nil
}

// inPlaceImage refuses an image node whose token cannot be rewritten in place
// without changing more than this one image.
func inPlaceImage(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.AliasNode:
		return fmt.Errorf("the image is a YAML alias (*%s): changing it means changing its anchor, which other nodes share", n.Value)
	case n.Anchor != "":
		return fmt.Errorf("the image is anchored (&%s): other nodes may alias it, so it is not rewritten", n.Anchor)
	case n.Kind != yaml.ScalarNode:
		return fmt.Errorf("the image is not a scalar")
	case n.Style != 0 && n.Style != yaml.SingleQuotedStyle && n.Style != yaml.DoubleQuotedStyle:
		return fmt.Errorf("the image is a tagged, block or flow scalar, which is not rewritten in place")
	case n.ShortTag() != "!!str":
		return fmt.Errorf("the image %q is not a string (%s)", n.Value, n.ShortTag())
	}
	return nil
}

func (p *writePlan) planLiteral(loc imageLoc, svc, img string) error {
	f, n := loc.file, loc.node
	quote := ""
	switch n.Style {
	case yaml.SingleQuotedStyle:
		quote = "'"
	case yaml.DoubleQuotedStyle:
		quote = `"`
	}
	start, ok := byteOffset(f.original, n.Line, n.Column)
	token := quote + n.Value + quote
	if !ok || !bytes.HasPrefix(f.original[start:], []byte(token)) {
		return fmt.Errorf("the image %q in %s is spelled with escapes or line folding and is not rewritten in place", n.Value, f.path)
	}
	return p.addEdit(f, &textEdit{
		start: start, end: start + len(token), text: quote + img + quote, owners: []string{svc},
		imageCheck: &yamlImagePath{doc: loc.doc, service: svc, image: img, style: n.Style},
	})
}

var (
	// ${VAR}, ${VAR:-default}, ${VAR-default}, ${VAR:?err}, ${VAR?err}, $VAR — the
	// whole value. A default holding `$` or braces (nesting) is not matched.
	wholeVarRe = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)(?:(?::?-|:?\?)[^${}]*)?\}|([A-Za-z_][A-Za-z0-9_]*))$`)
	// <anything>:<one variable as the whole tag>.
	trailingTagRe = regexp.MustCompile(`^(.+):(?:\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(?::?-|:?\?)[^${}:/@]*)?\}|\$([A-Za-z_][A-Za-z0-9_]*))$`)
)

func wholeVarRef(raw string) (string, bool) {
	m := wholeVarRe.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	return m[1] + m[2], true
}

func trailingTagVar(raw string) (string, bool) {
	m := trailingTagRe.FindStringSubmatch(raw)
	if m == nil || strings.Contains(m[1], "@") {
		return "", false
	}
	return m[2] + m[3], true
}

// tagChange returns the tag that turns current into img, when that is the only
// difference: current's exact spelling up to its tag must prefix img.
func tagChange(current, img string) (string, error) {
	named, err := reference.ParseNormalizedNamed(current)
	if err != nil {
		return "", fmt.Errorf("the current image %q does not parse: %w", current, err)
	}
	tagged, ok := named.(reference.NamedTagged)
	if _, digested := named.(reference.Digested); !ok || digested {
		return "", fmt.Errorf("the current image %q is not a plain name:tag", current)
	}
	prefix, ok := strings.CutSuffix(current, ":"+tagged.Tag())
	if !ok {
		return "", fmt.Errorf("the current image %q does not end in its tag", current)
	}
	tag, ok := strings.CutPrefix(img, prefix+":")
	if !ok {
		return "", fmt.Errorf("%q is not %s with a different tag", img, prefix)
	}
	if _, err := reference.WithTag(reference.TrimNamed(named), tag); err != nil || strings.ContainsAny(tag, ":@/") {
		return "", fmt.Errorf("%q is not %s with a different tag", img, prefix)
	}
	return tag, nil
}

// maxTemplateDepth bounds a chain of env values that are themselves variables.
const maxTemplateDepth = 8

// planVar sets name to value in the env file compose takes name from. current is
// the image the variable resolves to today when it holds a whole image, "" when
// it holds a tag.
//
// An assignment whose value is itself a template is not flattened into a literal:
// `IMMICH_SERVER_IMAGE=…immich-server:${IMMICH_VERSION:-release}` couples the
// server to the machine-learning image through IMMICH_VERSION, and writing a
// literal there would silently decouple them. The same rule as a compose-level
// template applies: a whole-value variable passes the value on; a single trailing
// tag variable takes the new tag — once, so services sharing it move together, and
// a service that would diverge fails the reload verification; anything else is
// refused. A single-quoted value is literal to dotenv and is not a template.
//
// A variable no env file declares, reached through such a template, is declared
// just above the first statement that uses it — appended at the end it would come
// after dotenv had already expanded the template with the default.
func (p *writePlan) planVar(name, value, svc, current string, depth int, use *envUse) error {
	if !safeEnvLineValue(value) || strings.ContainsAny(value, "#'\"\\$ ") {
		return fmt.Errorf("refusing to write %q to %s", echo(value), name)
	}
	if v, ok := p.vars[name]; ok {
		if v.value != value {
			return fmt.Errorf("%s would set %s to %q, but %s sets it to %q — they share the variable",
				svc, name, value, strings.Join(v.edit.owners, ", "), v.value)
		}
		v.edit.owners = append(v.edit.owners, svc)
		if v.inserted && use != nil && (v.use == nil || use.before(v.use)) {
			return p.moveDeclaration(name, v, use)
		}
		return nil
	}
	if _, ok := os.LookupEnv(name); ok {
		return fmt.Errorf("%s is set in the agent's own environment, which compose prefers to every env file; writing it would change nothing", name)
	}
	f, stmt, err := p.assignmentFor(name)
	if err != nil {
		return err
	}
	if stmt != nil && stmt.quote != '\'' {
		if raw := string(f.original[stmt.valueStart:stmt.valueEnd]); strings.Contains(raw, "$") {
			if depth >= maxTemplateDepth {
				return fmt.Errorf("%s is one of more than %d variables naming each other; not followed further", name, maxTemplateDepth)
			}
			here := p.useOf(f, stmt)
			if inner, ok := wholeVarRef(raw); ok {
				return p.planVar(inner, value, svc, current, depth+1, here)
			}
			if inner, ok := trailingTagVar(raw); ok && current != "" {
				tag, err := tagChange(current, value)
				if err != nil {
					return fmt.Errorf("%s in %s is %q, which varies only its tag (${%s}): %w", name, f.path, raw, inner, err)
				}
				return p.planVar(inner, tag, svc, "", depth+1, here)
			}
			return fmt.Errorf("%s in %s is itself a template (%q) beyond a whole-value or single trailing tag variable; "+
				"it is not flattened into a literal", name, f.path, raw)
		}
	}
	edit := &textEdit{owners: []string{svc}}
	v := &varEdit{value: value, edit: edit, file: f}
	switch {
	case stmt != nil:
		edit.start, edit.end, edit.text = stmt.valueStart, stmt.valueEnd, value
	case use != nil:
		f, v.file, v.use, v.inserted = use.file, use.file, use, true
		edit.start, edit.end, edit.text = use.offset, use.offset, name+"="+value+lineEnding(f.original)
	default:
		v.inserted = true
		edit.start, edit.end = len(f.original), len(f.original)
		edit.text = p.appendText(f, name+"="+value)
	}
	if err := p.addEdit(f, edit); err != nil {
		return err
	}
	p.vars[name] = v
	return nil
}

// useOf is the position of stmt's line in f, as an envUse.
func (p *writePlan) useOf(f *fileState, stmt *envStatement) *envUse {
	lineStart := bytes.LastIndexByte(f.original[:stmt.valueStart], '\n') + 1
	if lineStart == 0 && bytes.HasPrefix(f.original, envUTF8BOM) {
		lineStart = len(envUTF8BOM) // the byte order mark stays first
	}
	return &envUse{file: f, order: slices.Index(p.paths.env, f.path), offset: lineStart}
}

// moveDeclaration moves an inserted declaration of name up to an earlier use.
func (p *writePlan) moveDeclaration(name string, v *varEdit, use *envUse) error {
	v.file.edits = slices.DeleteFunc(v.file.edits, func(e *textEdit) bool { return e == v.edit })
	v.edit.start, v.edit.end = use.offset, use.offset
	v.edit.text = name + "=" + v.value + lineEnding(use.file.original)
	v.file, v.use = use.file, use
	return p.addEdit(use.file, v.edit)
}

func lineEnding(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// assignmentFor returns the env file and statement compose takes name from — the
// last statement, across the project's env files in order, that declares it.
// compose shares one environment across those files: a bare `NAME` line declares
// NAME only when the process environment or an earlier statement already has it.
// A nil statement means nothing declares it: the returned file is where a new
// assignment belongs (the last env file compose reads, or the working dir's .env,
// which compose reads by default once it exists).
func (p *writePlan) assignmentFor(name string) (*fileState, *envStatement, error) {
	declared := map[string]bool{}
	var (
		lastFile *fileState
		lastStmt *envStatement
	)
	for _, path := range p.paths.env {
		f, err := p.file(path)
		if err != nil {
			return nil, nil, err
		}
		if !f.existed {
			return nil, nil, fmt.Errorf("env file %s does not exist", path)
		}
		stmts, err := scanEnvStatements(f.original)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for i := range stmts {
			st := &stmts[i]
			if st.inherited {
				if _, inEnv := os.LookupEnv(st.key); !inEnv && !declared[st.key] {
					continue
				}
			}
			declared[st.key] = true
			if st.key == name {
				lastFile, lastStmt = f, st
			}
		}
	}
	switch {
	case lastStmt != nil && lastStmt.inherited:
		return nil, nil, fmt.Errorf("%s is declared in %s by a bare `%s` line that inherits its value; it is not rewritten", name, lastFile.path, name)
	case lastStmt != nil:
		return lastFile, lastStmt, nil
	case len(p.paths.env) > 0:
		f, err := p.file(p.paths.env[len(p.paths.env)-1])
		return f, nil, err
	case p.entry.WorkingDir != "":
		f, err := p.file(filepath.Join(p.entry.WorkingDir, ".env"))
		return f, nil, err
	}
	return nil, nil, fmt.Errorf("no env file to hold %s", name)
}

// appendText is line as a new last line of f: preceded by a line break when the
// file does not end with one, in the file's own line ending.
func (p *writePlan) appendText(f *fileState, line string) string {
	eol := lineEnding(f.original)
	appending := false
	for _, e := range f.edits {
		if e.start == len(f.original) {
			appending = true
		}
	}
	if len(f.original) > 0 && !bytes.HasSuffix(f.original, []byte("\n")) && !appending {
		return eol + line + eol
	}
	return line + eol
}

func (p *writePlan) addEdit(f *fileState, e *textEdit) error {
	for _, o := range f.edits {
		if e.start < o.end && o.start < e.end {
			return fmt.Errorf("two changes overlap in %s", f.path)
		}
	}
	f.edits = append(f.edits, e)
	return nil
}

// render applies every planned edit in memory and checks each rewritten compose
// file reads back as intended, before anything is written.
func (p *writePlan) render() ([]*fileState, error) {
	var out []*fileState
	for _, path := range p.order {
		f := p.files[path]
		if len(f.edits) == 0 {
			continue
		}
		f.written = applyEdits(f.original, f.edits)
		for _, e := range f.edits {
			if e.imageCheck == nil {
				continue
			}
			if err := readsBack(f.written, *e.imageCheck); err != nil {
				return nil, fmt.Errorf("%s: %w", f.path, err)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// readsBack checks that, in data, the addressed image reads as exactly the
// intended string in the same style — a spelling that parses differently in
// place (a trailing ':' in a plain scalar, a number) is refused, not re-encoded.
func readsBack(data []byte, want yamlImagePath) error {
	docs, err := decodeYAMLDocs(data)
	if err != nil {
		return fmt.Errorf("%q written in place would not parse: %w", want.image, err)
	}
	if want.doc >= len(docs) {
		return fmt.Errorf("%q written in place changes the document structure", want.image)
	}
	n, err := serviceImageNode(docs[want.doc], want.service)
	if err != nil || n == nil || n.Value != want.image || n.ShortTag() != "!!str" || n.Style != want.style {
		return fmt.Errorf("%q written in place would not read back as that image; it is not rewritten", want.image)
	}
	return nil
}

// applyEdits returns original with edits applied; insertions at one offset keep
// their plan order.
func applyEdits(original []byte, edits []*textEdit) []byte {
	sorted := slices.Clone(edits)
	slices.SortStableFunc(sorted, func(a, b *textEdit) int { return a.start - b.start })
	var buf bytes.Buffer
	pos := 0
	for _, e := range sorted {
		buf.Write(original[pos:e.start])
		buf.WriteString(e.text)
		pos = e.end
	}
	buf.Write(original[pos:])
	return buf.Bytes()
}

// byteOffset converts yaml's 1-based line and rune column to a byte offset.
func byteOffset(src []byte, line, column int) (int, bool) {
	off := 0
	for l := 1; l < line; l++ {
		i := bytes.IndexByte(src[off:], '\n')
		if i < 0 {
			return 0, false
		}
		off += i + 1
	}
	for c := 1; c < column; c++ {
		if off >= len(src) || src[off] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRune(src[off:])
		off += size
	}
	return off, off <= len(src)
}

// mapValue returns the value node for key in a YAML mapping node (nil if absent).
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// verification
// ---------------------------------------------------------------------------

// verifyImages checks that after differs from before in exactly the target
// services' images.
func verifyImages(before, after *types.Project, targets map[string]string) error {
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(after.Services)) {
		a := after.Services[name]
		b, ok := before.Services[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("service %s appeared", name))
			continue
		}
		want, targeted := targets[name]
		if !targeted {
			want = b.Image
		}
		if a.Image != want {
			if targeted {
				problems = append(problems, fmt.Sprintf("%s resolves to %q, not %q: something compose reads after the written value overrides it", name, a.Image, want))
			} else {
				problems = append(problems, fmt.Sprintf("%s's image changed from %q to %q", name, b.Image, a.Image))
			}
		}
		a.Image = b.Image
		if diff := serviceDiff(b, a); diff != "" {
			problems = append(problems, fmt.Sprintf("%s's %s changed", name, diff))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before.Services)) {
		if _, ok := after.Services[name]; !ok {
			problems = append(problems, fmt.Sprintf("service %s disappeared", name))
		}
	}
	bp, ap := *before, *after
	bp.Services, ap.Services = nil, nil
	bp.Environment, ap.Environment = nil, nil
	if !jsonEqual(bp, ap) {
		problems = append(problems, "the project's networks, volumes, secrets or configs changed")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func serviceDiff(a, b types.ServiceConfig) string {
	for _, f := range []struct {
		name string
		x, y any
	}{
		{"environment", a.Environment, b.Environment},
		{"labels", a.Labels, b.Labels},
		{"volumes", a.Volumes, b.Volumes},
		{"networks", a.Networks, b.Networks},
		{"ports", a.Ports, b.Ports},
	} {
		if !jsonEqual(f.x, f.y) {
			return f.name
		}
	}
	if !jsonEqual(a, b) {
		return "configuration"
	}
	return ""
}

func jsonEqual(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// ---------------------------------------------------------------------------
// writing, restoring, reverting
// ---------------------------------------------------------------------------

// writeAll writes every rendered file; on a failure it restores what it had
// already written. The files are not one atomic unit — each is — and the
// project lock keeps any other job from reading them in between.
func (w *imageWrite) writeAll() error {
	for i, f := range w.files {
		if err := writeFileAtomic(f.path, f.written, f.mode); err != nil {
			if rerr := restoreFiles(w.files[:i]); rerr != nil {
				return fmt.Errorf("%w — and restoring the files already written failed: %v", err, rerr)
			}
			return err
		}
		f.present, f.current = true, f.written
	}
	return nil
}

// restoreAll puts every written file back to its original bytes.
func (w *imageWrite) restoreAll() error { return restoreFiles(w.files) }

func restoreFiles(files []*fileState) error {
	var errs []error
	for _, f := range files {
		errs = append(errs, restoreFile(f, f.original, f.existed))
	}
	return errors.Join(errs...)
}

// restoreFile writes content back, or removes the file when it should not exist.
func restoreFile(f *fileState, content []byte, exists bool) error {
	if exists {
		return writeFileAtomic(f.path, content, f.mode)
	}
	fi, err := os.Lstat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer the file the write created; not removed", f.path)
	}
	if err := os.Remove(f.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(f.path))
}

// revert undoes the write for services (nil: all of them). Each file is rebuilt
// from its original bytes with only the edits of services that keep their new
// image; a file the write created is removed when none remain. A file that
// changed since the write is left alone and reported. The result is verified: each
// reverted service must resolve to its image from before the write again.
func (w *imageWrite) revert(ctx context.Context, services []string, load projectLoader) error {
	if w == nil || len(w.files) == 0 {
		return nil
	}
	named := func(svc string) bool { return services == nil || slices.Contains(services, svc) }
	reverting := map[string]bool{}
	for svc := range w.targets {
		reverting[svc] = w.reverted[svc] || named(svc)
	}
	kept := map[string]string{}
	var problems []string
	for _, f := range w.files {
		var keep []*textEdit
		for _, e := range f.edits {
			if slices.ContainsFunc(e.owners, func(o string) bool { return !reverting[o] }) {
				keep = append(keep, e)
				for _, o := range e.owners {
					if reverting[o] && named(o) {
						problems = append(problems, fmt.Sprintf("%s keeps its new image: it shares an edit in %s with %s, which keeps its own",
							o, f.path, strings.Join(e.owners, ", ")))
					}
					kept[o] = w.targets[o]
				}
			}
		}
		onDisk, err := os.ReadFile(f.path)
		unchanged := (f.present && err == nil && bytes.Equal(onDisk, f.current)) ||
			(!f.present && errors.Is(err, os.ErrNotExist))
		if !unchanged {
			problems = append(problems, fmt.Sprintf("%s changed after the update wrote it and is left as it is", f.path))
			continue
		}
		content, present := applyEdits(f.original, keep), f.existed || len(keep) > 0
		if err := restoreFile(f, content, present); err != nil {
			problems = append(problems, fmt.Sprintf("restore %s: %v", f.path, err))
			continue
		}
		f.present, f.current = present, content
	}
	for svc, r := range reverting {
		if r {
			w.reverted[svc] = true
		}
	}
	after, err := load(ctx, w.entry)
	if err == nil {
		err = verifyImages(w.baseline, after, kept)
	}
	if err != nil {
		problems = append(problems, "after the revert: "+err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(slices.Compact(problems), "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// the atomic write
// ---------------------------------------------------------------------------

// writeFileAtomic replaces path's content with data, so a crash leaves either
// the old file or the new one, never a torn or empty one:
//
//   - The temp file is created with os.CreateTemp in the target's directory:
//     O_EXCL and a random name, so a file, symlink or directory planted under a
//     predictable temp name is neither followed nor removed.
//   - It is synced before the rename, and the directory after it.
//   - A symlink is written through: the rename lands on the file it points to, not
//     over the link. An existing file keeps its permission bits and owner; mode
//     applies only to a file this creates.
//   - The rename gives the path a new inode, so another hard link to the old file
//     keeps the old content. No compose file here is hard-linked; a hard-linked one
//     would stop being shared.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	st, err := os.Stat(target)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if existed {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create a temp file in %s: %w", dir, err)
	}
	if err := fillSynced(tmp, data, mode, st); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename %s: %w", target, err)
	}
	return syncDir(dir)
}

// fillSynced writes data to a fresh temp file, gives it mode and — when like is
// non-nil — like's owner, syncs and closes it.
func fillSynced(f *os.File, data []byte, mode os.FileMode, like os.FileInfo) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if sys, ok := statOwner(like); ok && err == nil {
		err = f.Chown(int(sys.Uid), int(sys.Gid))
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// syncDir makes a rename or removal in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func statOwner(fi os.FileInfo) (*syscall.Stat_t, bool) {
	if fi == nil {
		return nil, false
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	return sys, ok
}
