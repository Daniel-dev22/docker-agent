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

// validImageRef reports whether s is an image reference the agent may pull and
// write into a compose file or .env as a single token.
//
// Two checks, in order. The charset above refuses what would corrupt a FILE —
// whitespace, control characters, `$`, quotes, shell metacharacters — by
// construction, before any grammar is consulted. Then the grammar is docker's own:
// github.com/distribution/reference, the parser compose and the daemon use. A
// hand-written approximation of it accepted `traefik:`, `traefik@` and
// `traefik:v3:x`, each of which the daemon then refuses at pull time — after the
// value had already been written to the stack's files.
//
// An image ID (`sha256:<hex>`, or the bare 64-hex form the daemon also accepts) is
// refused (isImageID). It names an image in one daemon's local store, not
// something a registry can resolve: a stack pinned to one cannot be checked,
// pulled, or deployed on another host, and nothing records which tag it came from.
func validImageRef(s string) bool {
	if s == "" || len(s) > maxImageRefLen {
		return false
	}
	for _, r := range s {
		if !allowedImageRefRune(r) {
			return false
		}
	}
	ref, err := reference.ParseAnyReference(s)
	if err != nil {
		return false
	}
	_, named := ref.(reference.Named)
	return named
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
