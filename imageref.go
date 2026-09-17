package main

import (
	"fmt"

	"github.com/distribution/reference"
)

// Validation for an operator-supplied image reference.
//
// The override_image path is the one place a caller hands the agent a string that
// is written into files on disk and passed to `docker pull`. Until now it was
// checked only for emptiness — while the far less dangerous Frigate BRANCH field,
// two repos away, has always been regex-guarded. This closes that asymmetry.
//
// 🔴 The sharp end is the writer (tagwrite.go). When a compose service's image is a
// `${VAR}` reference the override is written as the value of a `KEY=value` line in
// the project's .env, so a value containing a newline would append further
// environment lines to the stack — every one of which compose honours on the next
// `up`.
const maxImageRefLen = 512

// fileSafeImageRune is the alphabet of docker's reference grammar: registry host
// (including an IPv6 literal's brackets), port, repository path, tag and digest.
// Everything that could corrupt a file falls outside it by construction —
// whitespace and control characters, `$` (which compose interpolates in a .env
// value), quotes, backslash, backtick and the shell metacharacters.
func fileSafeImageRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-', r == '/', r == ':', r == '@', r == '+', r == '[', r == ']':
		return true
	}
	return false
}

// validImageRef reports whether s is an image reference the agent may pull and
// write into a compose file or .env as a single token.
//
// It agrees exactly with docker's own grammar — distribution/reference's
// ParseNormalizedNamed, the parser compose and the daemon use — plus three extra
// rules, each checked on its own and each stricter, never looser:
//
//  1. Not an image ID (isImageID): `sha256:<hex>` parses as a repository named
//     "sha256", but the daemon reads it as an ID, which names an image in one
//     daemon's store — not checkable, pullable or portable, and silent about which
//     tag it came from.
//  2. At most maxImageRefLen bytes.
//  3. Every rune in fileSafeImageRune. The grammar admits nothing outside it today;
//     the rule keeps the file-safety guarantee independent of the parser's version.
//
// A hand-written approximation of the grammar accepted `traefik:`, `traefik@` and
// `traefik:v3:x` — each refused by the daemon at pull time, after the value had
// been written into the stack's files — and refused registry hosts written as IPv6
// literals (`[::1]:5000/app`). TestValidImageRefAgreesWithTheGrammar holds the two
// together.
func validImageRef(s string) bool {
	if s == "" || len(s) > maxImageRefLen {
		return false
	}
	for _, r := range s {
		if !fileSafeImageRune(r) {
			return false
		}
	}
	if isImageID(s) {
		return false
	}
	_, err := reference.ParseNormalizedNamed(s)
	return err == nil
}

// isImageID reports whether s is an image ID rather than a named reference.
//
// The legacy Portainer rollback (Ansible, retired with the Portainer migration)
// wrote a container's image ID into the stack's CURRENT_<NAME>_IMAGE, and the
// migration copied that environment into the stack's .env verbatim. esphome on
// kd-nuc01 and ng-nuc01 still carry it, and every consumer of the value — the
// image check, the update resolver, the writer — must say so rather than treat
// `sha256` as a repository name.
func isImageID(s string) bool {
	ref, err := reference.ParseAnyReference(s)
	if err != nil {
		return false
	}
	_, named := ref.(reference.Named)
	return !named
}

// imageIDError explains, for one service, why an image ID cannot be checked or
// updated, and what to do instead.
func imageIDError(service, image string) error {
	return fmt.Errorf("%s runs the local image ID %s, not an image reference: it cannot be checked "+
		"against a registry, pulled, or moved to another host — deploy a tag with override_image "+
		"(the tag the stack is meant to track)", service, clipTo(image, 19))
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
