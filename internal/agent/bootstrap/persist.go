// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Persist writes the bootstrap result to disk atomically, in the order
//
//  1. key (mode 0600 — secret, owner-only)
//  2. cert (mode 0644)
//  3. CA (mode 0644)
//
// Each file is written via a tempfile + rename so partial writes are
// not observed by concurrent readers. The set-wide invariant ("all
// three present, or none") is enforced by the boot orchestrator's
// polling-loop check on subsequent agent start - a failure between
// files 1 and 3 leaves detectable partial state that the polling loop
// logs at WARN and does not transition to State B.
//
// The agent identifies itself by name: it reads its identity (node
// name) from the cert CN at startup; the CP-assigned UUID is no longer
// needed agent-side. `Result.NodeID` remains carried for logging
// (operators can correlate with CP-side records via the log line).
func Persist(certPath, keyPath, caPath string, result *Result) error {
	if result == nil {
		return errors.New("bootstrap: Persist called with nil result")
	}
	if err := writeFileAtomic(keyPath, result.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("write key %s: %v", keyPath, err)
	}
	if err := writeFileAtomic(certPath, result.CertPEM, 0o644); err != nil {
		return fmt.Errorf("write cert %s: %v", certPath, err)
	}
	if err := writeFileAtomic(caPath, result.CACertPEM, 0o644); err != nil {
		return fmt.Errorf("write ca %s: %v", caPath, err)
	}
	if err := handToServiceUser(keyPath, certPath, caPath); err != nil {
		return fmt.Errorf("hand cert material to service user: %v", err)
	}
	return nil
}

// handToServiceUser hands the freshly-written cert material to the user that
// owns the state directory it lives in. bootstrap is normally run as root
// (operators invoke `sudo otherix-agent bootstrap`), but the agent service runs
// as an unprivileged, dedicated user (the package's `otherix`). Without this,
// the service cannot read its own 0600 key or traverse the 0750 cert dir, and
// stays stuck waiting for cert material.
//
// The target owner is derived from the cert dir's parent - the package creates
// the state root (/var/lib/otherix) owned by the service user - so there is no
// hard-coded username. It is a deliberate no-op when not running as root (a
// dev / non-package run already creates files as the right user) or when that
// parent is itself root-owned (nothing to hand off).
func handToServiceUser(paths ...string) error {
	if os.Geteuid() != 0 || len(paths) == 0 {
		return nil
	}
	dir := filepath.Dir(paths[0])
	uid, gid, ok, err := resolveServiceOwner(dir)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	// Chown the directory and each file. Lchown does not follow symlinks -
	// defense in depth, though the cert dir is not world-writable.
	for _, p := range append([]string{dir}, paths...) {
		if err := os.Lchown(p, uid, gid); err != nil {
			return fmt.Errorf("chown %s to %d:%d: %v", p, uid, gid, err)
		}
	}
	return nil
}

// resolveServiceOwner returns the uid/gid that owns certDir's parent (the
// service state root) and whether cert material should be handed to it. ok is
// false when the parent is root-owned (a dev / non-package layout where there
// is no separate service user to hand off to).
func resolveServiceOwner(certDir string) (uid, gid int, ok bool, err error) {
	parent := filepath.Dir(certDir)
	fi, statErr := os.Stat(parent)
	if statErr != nil {
		return 0, 0, false, fmt.Errorf("stat state dir %s: %v", parent, statErr)
	}
	st, isUnix := fi.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false, nil
	}
	uid, gid = int(st.Uid), int(st.Gid)
	if uid == 0 && gid == 0 {
		return 0, 0, false, nil
	}
	return uid, gid, true, nil
}

// writeFileAtomic writes content to path through a tempfile + rename
// so concurrent readers (or a crash mid-write) never see a partial
// file. The tempfile lives in the same directory as the target so the
// rename stays atomic — cross-device renames degrade to copy + replace,
// which loses the atomicity guarantee.
//
// The parent directory is created with mode 0750 if absent (group-
// readable, world-not — enough for a dedicated agent process running
// as its own uid/gid; the individual file modes (0600 for key, 0644
// for cert / CA) are what govern actual content access). Mode on the
// tempfile is applied via Chmod before the rename so the final file
// lands on disk with the requested permissions from moment one.
func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("ensure parent dir %s: %v", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %v", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %v", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %v", tmpName, path, err)
	}
	return nil
}
