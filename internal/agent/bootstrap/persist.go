// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Persist writes the bootstrap result к disk atomically, в the order
//
//  1. key (mode 0600 — secret, owner-only)
//  2. cert (mode 0644)
//  3. CA (mode 0644)
//
// Each file is written via а tempfile + rename so partial writes are
// не observed by concurrent readers. The set-wide invariant ("all
// three present, или none") is enforced by the boot orchestrator's
// polling-loop check on subsequent agent start - а failure between
// files 1 и 3 leaves detectable partial state что the polling loop
// logs at WARN и does not transition к State B.
//
// The agent identifies itself by name: it reads its identity (node
// name) от the cert CN at startup; the CP-assigned UUID is no longer
// needed agent-side. `Result.NodeID` остаётся carried for logging
// (operators can correlate с CP-side records via the log line).
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
	return nil
}

// writeFileAtomic writes content к path through а tempfile + rename
// so concurrent readers (или а crash mid-write) never see а partial
// file. The tempfile lives в the same directory as the target so the
// rename stays atomic — cross-device renames degrade к copy + replace,
// which loses the atomicity guarantee.
//
// The parent directory is created с mode 0750 если absent (group-
// readable, world-not — enough for а dedicated agent process running
// as its own uid/gid; the individual file modes (0600 для key, 0644
// для cert / CA) are what govern actual content access). Mode on the
// tempfile is applied via Chmod before the rename so the final file
// lands on disk с the requested permissions от moment one.
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
