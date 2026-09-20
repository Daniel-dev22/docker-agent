package main

// Where a stack's owner is recorded, and why projects.json is not enough.
//
// Phase 2 put ProjectEntry.Owner in the central index, which made the owner a
// fact with exactly one copy, held in a file that is not the thing it describes.
// Measured on a throwaway agent (2026-09-20): with an owner:"ansible" stack
// running, `rm projects.json` + restart brought it back `owner:null,
// managed:true` — enrichFromLive re-adopts from container labels, which carry no
// owner — and the unowned entry was PERSISTED. The editor's own call (register
// with files, no owner) then rewrote the compose file on disk; with the owner
// recorded the identical call is refused `project_owned`. The same laundering
// needed no file loss at all: DELETE /v1/projects/<name> made no owner check and
// the next fleet snapshot read `managed:true` with the containers still up.
//
// So the owner is recorded in the DIRECTORY it governs. That is not a second
// declaration — there is still exactly one, the `owner` on the register call —
// only a durable record kept beside the files instead of in an index that can be
// lost separately from them. ProjectEntry.Owner remains the wire value and an
// in-memory cache, refreshed from the directory at the two moments an entry
// enters the index (registry load, adoption) and re-read before any write.
//
// Same shape as underComposeRoot: a property of the path. Nothing to migrate,
// and nothing a future owned stack has to remember to set — the register it
// already makes is what writes it.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ownerMarkName is the agent's own metadata file inside a stack directory.
	// Never served by /bundle (which serves declared compose and env files),
	// never copied by a cross-host copy (same reason), and rejected as a bundle
	// path so a write cannot forge one.
	ownerMarkName = ".docker-agent-owner"
	// maxOwnerMarkBytes bounds the read. The file holds a tool name, and a
	// working dir is host state the agent does not otherwise control.
	maxOwnerMarkBytes = 128
	// unknownOwner is what a mark that exists but cannot be read as a valid owner
	// resolves to. Fail-safe: a directory that says something about who renders it
	// and cannot be understood is not one the agent may rewrite. It is itself a
	// valid owner name, so it needs no special case on the wire, in a refusal
	// message, or in the frontend.
	unknownOwner = "unknown"
)

func ownerMarkPath(dir string) string { return filepath.Join(dir, ownerMarkName) }

// readOwnerMark returns the owner dir records, or "" when it records none.
//
// Ownership exists only under the compose root: outside it the agent can neither
// read nor rewrite the files, so there is nothing to protect and nothing to read.
//
// It returns a USABLE value alongside any error — a mark that is present but
// unreadable or malformed resolves to unknownOwner — so a caller that ignores the
// error still fails safe. The error is for the callers that run once (registry
// load, adoption, register) to log; the snapshot path ignores it deliberately,
// because a bad mark must not log on every cycle.
func readOwnerMark(dir, root string) (string, error) {
	if !underComposeRoot(dir, root) {
		return "", nil
	}
	p := ownerMarkPath(dir)
	// Symlinks resolved: a mark linking out of the root is not this stack's.
	if err := confinePath(p, root); err != nil {
		return unknownOwner, fmt.Errorf("owner mark %s: %w", echo(p), err)
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return unknownOwner, fmt.Errorf("open owner mark %s: %w", echo(p), err)
	}
	defer f.Close()
	// The read is bounded, so a huge file costs one page, not its size. There is
	// deliberately no separate "too large" branch: maxOwnerMarkBytes is far above
	// maxOwnerLen, so anything oversized fails the validity check below — a branch
	// that can never decide is a branch no test can cover.
	buf, err := io.ReadAll(io.LimitReader(f, maxOwnerMarkBytes+1))
	if err != nil {
		return unknownOwner, fmt.Errorf("read owner mark %s: %w", echo(p), err)
	}
	owner := strings.TrimSpace(string(buf))
	// An EMPTY mark is not "no owner": clearing an owner REMOVES the file, so a
	// mark that exists and names nobody is a truncated write, not a decision.
	if owner == "" || !validOwner(owner) || len(owner) > maxOwnerLen {
		return unknownOwner, fmt.Errorf("owner mark %s does not name a valid owner", echo(p))
	}
	return owner, nil
}

// writeOwnerMark records owner in dir, or removes the mark when owner is "".
//
// A no-op for a directory outside the compose root: the agent does not write
// there, and a stack outside the root is never editable, so nothing depends on
// the record.
func writeOwnerMark(dir, root, owner string) error {
	if !underComposeRoot(dir, root) {
		return nil
	}
	p := ownerMarkPath(dir)
	if err := confinePath(p, root); err != nil {
		return fmt.Errorf("owner mark %s: %w", echo(p), err)
	}
	if owner == "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("clear owner mark %s: %w", echo(p), err)
		}
		return nil
	}
	if !validOwner(owner) || len(owner) > maxOwnerLen {
		return fmt.Errorf("refusing to record %s as an owner", echo(owner))
	}
	// Atomic (tmp + fsync + rename), so a crash mid-write cannot leave a mark
	// that names nobody — which readOwnerMark would have to treat as unknown.
	return writeFileAtomic(p, []byte(owner+"\n"), 0o600)
}
