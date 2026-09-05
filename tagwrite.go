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
//   - `image: ${VAR}` in the compose file → update VAR in the project's .env
//     (the var holds the full image ref).
//   - `image: literal:tag` → rewrite the scalar in the compose file, preserving
//     comments + key order (yaml.v3 Node round-trip).
//
// setServiceImage returns the PREVIOUS on-disk value so the engine can revert a
// single service on rollback (partial, per-container rollback safe).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// envVarRefRe extracts VAR from "${VAR}", "${VAR:-default}", "$VAR".
var envVarRefRe = regexp.MustCompile(`^\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// setServiceImage points service at newImage on disk and returns the prior value
// (for rollback). It updates the .env var when the compose image is a `${VAR}`
// reference, else rewrites the compose scalar in place.
func setServiceImage(entry ProjectEntry, service, newImage string) (oldImage string, err error) {
	files := entry.absComposeFiles()
	if len(files) == 0 {
		return "", fmt.Errorf("no compose files for project %q", entry.Name)
	}
	for _, f := range files {
		raw, found, varName, scalarSetter, ferr := findServiceImage(f, service)
		if ferr != nil {
			return "", ferr
		}
		if !found {
			continue
		}
		if varName != "" {
			// Image is ${VAR}: persist the full ref into the .env var. Keep the
			// compose scalar as-is.
			old, eerr := setEnvVar(entry, varName, newImage)
			if eerr != nil {
				return "", eerr
			}
			return old, nil
		}
		// Literal image: rewrite the scalar in place.
		if err := scalarSetter(newImage); err != nil {
			return "", err
		}
		return raw, nil
	}
	return "", fmt.Errorf("service %q image not found in compose files", service)
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
	var doc yaml.Node
	if uerr := yaml.Unmarshal(data, &doc); uerr != nil {
		return "", false, "", nil, fmt.Errorf("parse %s: %w", path, uerr)
	}
	if len(doc.Content) == 0 {
		return "", false, "", nil, nil
	}
	root := doc.Content[0]
	services := mapValue(root, "services")
	if services == nil {
		return "", false, "", nil, nil
	}
	svc := mapValue(services, service)
	if svc == nil {
		return "", false, "", nil, nil
	}
	imgNode := mapValue(svc, "image")
	if imgNode == nil || imgNode.Kind != yaml.ScalarNode {
		return "", false, "", nil, nil
	}
	raw = imgNode.Value
	if m := envVarRefRe.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
		varName = m[1]
	}
	setScalar = func(v string) error {
		imgNode.Value = v
		imgNode.Tag = "!!str"
		out, merr := yaml.Marshal(&doc)
		if merr != nil {
			return merr
		}
		return writeFileAtomic(path, out, 0o644)
	}
	return raw, true, varName, setScalar, nil
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

// envFilePath returns the .env path to use for a project (first declared env file,
// else <working_dir>/.env).
func envFilePath(entry ProjectEntry) string {
	for _, ef := range entry.EnvFiles {
		if filepath.IsAbs(ef) {
			return ef
		}
		if entry.WorkingDir != "" {
			return filepath.Join(entry.WorkingDir, ef)
		}
	}
	if entry.WorkingDir != "" {
		return filepath.Join(entry.WorkingDir, ".env")
	}
	return ""
}

// setEnvVar sets key=value in the project's .env, preserving other lines + order,
// and returns the prior value ("" if the key was absent or the file is new).
func setEnvVar(entry ProjectEntry, key, value string) (old string, err error) {
	path := envFilePath(entry)
	if path == "" {
		return "", fmt.Errorf("no .env path for project %q", entry.Name)
	}
	// 🔴 Refuse before touching the file. A newline in the value would append
	// further KEY=value lines that compose honours on the next `up`, and this
	// writer must hold for every caller rather than trusting the one boundary that
	// happens to validate today.
	if !safeEnvLineValue(key) || !safeEnvLineValue(value) {
		return "", fmt.Errorf("refusing to write %s: value contains a newline or control character", key)
	}
	var lines []string
	if data, rerr := os.ReadFile(path); rerr == nil {
		lines = strings.Split(string(data), "\n")
	} else if !os.IsNotExist(rerr) {
		return "", fmt.Errorf("read %s: %w", path, rerr)
	}

	prefix := key + "="
	replaced := false
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), prefix) {
			old = strings.TrimPrefix(strings.TrimSpace(ln), prefix)
			lines[i] = key + "=" + value
			replaced = true
			break
		}
	}
	if !replaced {
		// Append before a trailing empty line if present, else at the end.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines[n-1] = key + "=" + value
			lines = append(lines, "")
		} else {
			lines = append(lines, key+"="+value)
		}
	}
	if err := writeFileAtomic(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return "", err
	}
	return old, nil
}

// writeFileAtomic writes via a temp file + rename so a crash mid-write can't
// corrupt the compose/.env file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
