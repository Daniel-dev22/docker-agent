package main

// Durable image-tag persistence for the stack-update engine.
//
// When a resolver bumps a service's image to a concrete new tag, the change is
// written back to the on-host compose/.env so it survives an agent restart or a
// manual `docker compose up` — the on-host files stay the source of truth, with
// no side-channel state. Moving-tag (registry-digest) services are NOT written
// here — their tag is unchanged; `pull` fetches the new digest.
//
// Two cases:
//   - `image: ${VAR}` in the compose file → update VAR in the env file compose
//     actually takes it from (the var holds the full image ref).
//   - `image: literal:tag` → rewrite the scalar in the compose file that defines
//     it: its token in place, every other byte untouched (replaceScalar), or — for
//     a token that cannot be rewritten in place — a yaml.v3 Node round-trip, which
//     keeps comments and key order but not formatting.
//
// Which files those are comes from resolveLoadPaths — the same files, in the same
// order, a compose load reads — and from compose's own precedence: a later compose
// file overrides an earlier one's service image, and a later env file overrides an
// earlier one's variable. Writing anywhere else leaves the running tag and the
// on-disk tag disagreeing on the next `compose up`.
//
// setServiceImage returns the PREVIOUS on-disk state so the engine can revert a
// single service on rollback (partial, per-container rollback safe) with
// restoreServiceImage — the state, not a lookalike value: an env var the write
// added is removed again, because an empty assignment is not an absent one
// (`${VAR-default}` takes the default only when VAR is unset).

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

// envVarRefRe extracts VAR from "${VAR}", "${VAR:-default}", "$VAR".
var envVarRefRe = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// priorImage is a service's image as it stood on disk before setServiceImage.
type priorImage struct {
	literal string         // a literal image: the prior scalar
	env     *envAssignment // a ${VAR} image: the variable's prior assignment
}

// setServiceImage points service at newImage on disk and returns the prior state
// (for rollback). It updates the env var when the compose image is a `${VAR}`
// reference, else rewrites the compose scalar in place.
func setServiceImage(entry ProjectEntry, service, newImage string) (priorImage, error) {
	paths, err := resolveLoadPaths(entry)
	if err != nil {
		return priorImage{}, err
	}
	// The last compose file that sets the image is the one compose uses.
	for i := len(paths.config) - 1; i >= 0; i-- {
		raw, found, varName, setScalar, ferr := findServiceImage(paths.config[i], service)
		if ferr != nil {
			return priorImage{}, ferr
		}
		if !found {
			continue
		}
		if varName != "" {
			// Image is ${VAR}: persist the full ref into the env var. Keep the
			// compose scalar as-is.
			prior, eerr := setEnvVar(paths, entry.WorkingDir, varName, newImage)
			if eerr != nil {
				return priorImage{}, eerr
			}
			return priorImage{env: &prior}, nil
		}
		if err := setScalar(newImage); err != nil {
			return priorImage{}, err
		}
		return priorImage{literal: raw}, nil
	}
	return priorImage{}, fmt.Errorf("service %q image not found in compose files", service)
}

// restoreServiceImage puts back the on-disk state setServiceImage replaced.
func restoreServiceImage(entry ProjectEntry, service string, prior priorImage) error {
	if prior.env != nil {
		return prior.env.restore()
	}
	_, err := setServiceImage(entry, service, prior.literal)
	return err
}

// findServiceImage parses a compose file and locates services.<service>.image.
// Returns the raw scalar value, whether it was found, the referenced env var name
// (when the value is "${VAR}"), and a setter that rewrites the scalar + the file.
func findServiceImage(path, service string) (raw string, found bool, varName string, setScalar func(string) error, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return "", false, "", nil, nil
		}
		return "", false, "", nil, fmt.Errorf("read %s: %w", path, rerr)
	}
	raw, found, doc, imgNode, perr := imageIn(data, service)
	if perr != nil {
		return "", false, "", nil, fmt.Errorf("parse %s: %w", path, perr)
	}
	if !found {
		return "", false, "", nil, nil
	}
	if m := envVarRefRe.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
		varName = m[1]
	}
	setScalar = func(v string) error {
		if out, ok := replaceScalar(data, imgNode, v); ok {
			if again, found, _, _, _ := imageIn(out, service); found && again == v {
				return writeFileAtomic(path, out, 0o644)
			}
		}
		imgNode.Value = v
		imgNode.Tag = "!!str"
		out, merr := yaml.Marshal(doc)
		if merr != nil {
			return merr
		}
		return writeFileAtomic(path, out, 0o644)
	}
	return raw, true, varName, setScalar, nil
}

// imageIn locates services.<service>.image in a compose document.
func imageIn(data []byte, service string) (value string, found bool, doc *yaml.Node, image *yaml.Node, err error) {
	doc = &yaml.Node{}
	if err := yaml.Unmarshal(data, doc); err != nil {
		return "", false, nil, nil, err
	}
	if len(doc.Content) == 0 {
		return "", false, doc, nil, nil
	}
	svc := mapValue(mapValue(doc.Content[0], "services"), service)
	image = mapValue(svc, "image")
	if image == nil || image.Kind != yaml.ScalarNode {
		return "", false, doc, nil, nil
	}
	return image.Value, true, doc, image, nil
}

// replaceScalar rewrites one scalar's token in src and leaves every other byte as
// it was — a re-encode would re-indent the file and drop its blank lines on every
// update. It reports false unless the token at the node's position is exactly the
// plain, single- or double-quoted text node.Value spells (no escapes, no tag, not a
// block scalar) and v can take its place in the same style; the caller then
// re-parses the result and re-encodes unless it reads v.
func replaceScalar(src []byte, n *yaml.Node, v string) ([]byte, bool) {
	var quote string
	switch n.Style {
	case 0:
	case yaml.SingleQuotedStyle:
		quote = "'"
	case yaml.DoubleQuotedStyle:
		quote = `"`
	default:
		return nil, false
	}
	if !plainImageText.MatchString(v) {
		return nil, false
	}
	start, ok := byteOffset(src, n.Line, n.Column)
	if !ok {
		return nil, false
	}
	token := quote + n.Value + quote
	if !strings.HasPrefix(string(src[start:]), token) {
		return nil, false
	}
	out := make([]byte, 0, len(src)-len(token)+len(v)+2*len(quote))
	out = append(out, src[:start]...)
	out = append(out, quote+v+quote...)
	return append(out, src[start+len(token):]...), true
}

// plainImageText is text that means itself in any of the three scalar styles:
// no quote, escape, whitespace, comment or flow indicator, and not starting with
// one of YAML's indicator characters.
var plainImageText = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/:@+-]*$`)

// byteOffset converts yaml.v3's 1-based line and rune column to a byte offset.
func byteOffset(src []byte, line, column int) (int, bool) {
	off := 0
	for l := 1; l < line; l++ {
		i := strings.IndexByte(string(src[off:]), '\n')
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

// envFileForVar returns the env file compose takes key from: the LAST of the
// project's env files (resolveLoadPaths) that declares it — compose reads them in
// order and a later file overrides an earlier one. When none declares it, the
// assignment belongs in the last env file compose reads or — when it reads none —
// the working dir's .env, which compose reads by default once it exists.
//
// Declarations are found with compose's own dotenv parser and the lookup
// dotenv.GetEnvFromFile gives it (process environment, then the files read so
// far), so a bare `KEY` that inherits a value declares it exactly when compose
// says so, and a file compose cannot parse is refused rather than guessed at.
func envFileForVar(paths loadPaths, workingDir, key string) (string, error) {
	osEnv := types.NewMapping(os.Environ())
	readSoFar := map[string]string{}
	lookup := func(k string) (string, bool) {
		if v, ok := osEnv[k]; ok {
			return v, true
		}
		v, ok := readSoFar[k]
		return v, ok
	}
	declaredIn := ""
	for _, f := range paths.env {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", f, err)
		}
		vars, err := dotenv.UnmarshalBytesWithLookup(data, lookup)
		if err != nil {
			return "", fmt.Errorf("parse %s: %w", f, err)
		}
		if _, ok := vars[key]; ok {
			declaredIn = f
		}
		maps.Copy(readSoFar, vars)
	}
	switch {
	case declaredIn != "":
		return declaredIn, nil
	case len(paths.env) > 0:
		return paths.env[len(paths.env)-1], nil
	case workingDir != "":
		return filepath.Join(workingDir, ".env"), nil
	}
	return "", fmt.Errorf("no env file to hold %s", key)
}

// envAssignment is a variable's plain KEY= line in one env file as it stood
// before setEnvVar wrote it.
type envAssignment struct {
	file, key string
	line      string // the whole line, byte for byte
	assigned  bool   // the file had a plain KEY= line
	existed   bool   // the file existed
}

// setEnvVar sets key=value in the env file compose takes key from (envFileForVar),
// preserving other lines + order, and returns what it replaced.
func setEnvVar(paths loadPaths, workingDir, key, value string) (envAssignment, error) {
	// 🔴 Refuse before touching the file. A newline in the value would append
	// further KEY=value lines that compose honours on the next `up`, and this
	// writer must hold for every caller rather than trusting the one boundary that
	// happens to validate today.
	if !safeEnvLineValue(key) || !safeEnvLineValue(value) {
		return envAssignment{}, fmt.Errorf("refusing to write %s: value contains a newline or control character", key)
	}
	path, err := envFileForVar(paths, workingDir, key)
	if err != nil {
		return envAssignment{}, err
	}
	lines, existed, err := readEnvLines(path)
	if err != nil {
		return envAssignment{}, err
	}
	prior := envAssignment{file: path, key: key, existed: existed}
	// Replace the LAST plain assignment — the one a parser keeps. With none (the
	// variable is unset, or declared in another form such as `export KEY=`),
	// append: a later line wins, so the append takes effect.
	if i := lastAssignment(lines, key); i >= 0 {
		prior.line, prior.assigned = lines[i], true
		lines[i] = key + "=" + value
	} else {
		lines = appendEnvLine(lines, key+"="+value)
	}
	if err := writeFileAtomic(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return envAssignment{}, err
	}
	return prior, nil
}

// restore puts the file back as setEnvVar found it: the original line, or no
// line at all — and no file, when setEnvVar created it.
func (a envAssignment) restore() error {
	lines, _, err := readEnvLines(a.file)
	if err != nil {
		return err
	}
	i := lastAssignment(lines, a.key)
	switch {
	case a.assigned && i >= 0:
		lines[i] = a.line
	case a.assigned:
		lines = appendEnvLine(lines, a.line)
	case i >= 0:
		lines = append(lines[:i], lines[i+1:]...)
	}
	content := strings.Join(lines, "\n")
	if !a.existed && content == "" {
		if err := os.Remove(a.file); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", a.file, err)
		}
		return nil
	}
	return writeFileAtomic(a.file, []byte(content), 0o600)
}

// readEnvLines splits an env file into lines; a missing file has none.
func readEnvLines(path string) (lines []string, existed bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return strings.Split(string(data), "\n"), true, nil
}

// lastAssignment is the index of the last plain KEY= line, or -1.
func lastAssignment(lines []string, key string) int {
	prefix := key + "="
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), prefix) {
			return i
		}
	}
	return -1
}

// appendEnvLine appends before a trailing empty line (the final newline) when
// there is one, else at the end.
func appendEnvLine(lines []string, line string) []string {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines[n-1] = line
		return append(lines, "")
	}
	return append(lines, line)
}

// writeFileAtomic replaces path's content via a synced temp file + rename, so a
// crash mid-write can't leave a torn or empty compose/.env file. Replacing the
// content changes nothing else about the file: a symlink is written through (the
// rename lands on the file it points to rather than replacing the link with a
// copy), and an existing file keeps its permission bits and owner. mode applies
// only to a file this creates.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	st, err := os.Stat(target)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if existed {
		mode = st.Mode().Perm()
	}
	tmp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".tmp")
	if err := writeSynced(tmp, data, mode, st); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", target, err)
	}
	return nil
}

// writeSynced writes data to a fresh file with mode (umask aside) and, when like
// is non-nil, like's owner, and syncs it.
func writeSynced(path string, data []byte, mode os.FileMode, like os.FileInfo) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if sys, ok := statOwner(like); ok {
		if err := f.Chown(int(sys.Uid), int(sys.Gid)); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func statOwner(fi os.FileInfo) (*syscall.Stat_t, bool) {
	if fi == nil {
		return nil, false
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	return sys, ok
}
