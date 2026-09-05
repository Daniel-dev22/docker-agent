package main

import "strings"

// Validation for an operator-supplied image reference.
//
// The override_image path is the one place a caller hands the agent a string that
// is written into files on disk and passed to `docker pull`. Until now it was
// checked only for emptiness — while the far less dangerous Frigate BRANCH field,
// two repos away, has always been regex-guarded. This closes that asymmetry.
//
// 🔴 The sharp end is setEnvVar. When a compose service's image is a `${VAR}`
// reference the override is persisted as a raw `KEY=value` line in the project's
// .env, so a value containing a newline appends further environment lines to the
// stack — every one of which compose will honour on the next `up`.
//
// The charset below is the UNION of what the OCI reference grammar permits across
// registry host, port, repository path, tag and digest. Everything dangerous falls
// outside it by construction rather than by enumeration: whitespace and control
// characters, `$` (which compose interpolates in a .env value), quotes, backslash,
// backtick, and the shell metacharacters — none of which can appear in a valid
// reference.
const maxImageRefLen = 512

// allowedImageRefRune is the union of the OCI grammar's character classes.
func allowedImageRefRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-', r == '/', r == ':', r == '@', r == '+':
		return true
	}
	return false
}

// validImageRef reports whether s is safe to pull and safe to write as a single
// line of a .env or a compose scalar.
//
// Deliberately NOT a full reference parser. Docker itself rejects a malformed
// reference safely, so the job here is to make the value harmless to the FILES it
// passes through, and to fail early with a clear message instead of late with a
// pull error. Over-strictness is the real risk: this accepts every one of the 33
// distinct image references running in the estate, including registry ports, ghcr
// paths, `@sha256:` digests and a bare digest.
func validImageRef(s string) bool {
	// No TrimSpace comparison: the charset below excludes every whitespace rune, so
	// a leading or trailing space is already a rejection. A trim check here was dead
	// code that read as protection. Callers facing a human (control-api) trim BEFORE
	// validating; this end refuses rather than silently altering what was asked for.
	if s == "" || len(s) > maxImageRefLen {
		return false
	}
	if r := rune(s[0]); !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
		return false // a reference starts with a host or a repository component
	}
	for _, r := range s {
		if !allowedImageRefRune(r) {
			return false
		}
	}
	// Structural sanity that the charset alone cannot express.
	if strings.Contains(s, "//") || strings.Count(s, "@") > 1 {
		return false
	}
	return true
}

// safeEnvLineValue reports whether v can be written as `KEY=v` without creating
// additional lines.
//
// Separate from validImageRef ON PURPOSE: setEnvVar is a general writer and must
// hold for every caller, present and future, not only for the one that happens to
// pass an image reference today. A guard that lives only at the boundary protects
// only the callers that existed when it was written.
func safeEnvLineValue(v string) bool {
	for _, r := range v {
		if r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
