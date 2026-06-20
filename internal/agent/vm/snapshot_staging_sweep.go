// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SweepSnapshotStaging removes orphaned snapshot staging temp files left under
// each registered pool's snapshots/.staging dir by an interrupted capture (an
// agent kill or power loss between materializing a temp and the normal-path
// remove). A file is removed when its modification time is older than
// now-maxAge; maxAge == 0 removes every staging file (the boot sweep, run while
// nothing is in flight). A positive maxAge spares a temp a live capture may
// still be writing. Subdirectories are skipped, and the durable content-
// addressed blobs and sidecars under snapshots/ proper are never touched (only
// the .staging dir is swept). An absent staging dir is a no-op. Fail-open per
// pool: an error reading or removing under one pool is aggregated and the sweep
// continues to the next pool, never aborting the others. Returns the total
// count removed and any aggregated per-pool error.
func (m *Manager) SweepSnapshotStaging(maxAge time.Duration) (removed int, err error) {
	m.poolsMu.RLock()
	roots := make([]string, 0, len(m.pools))
	for _, p := range m.pools {
		roots = append(roots, p.root)
	}
	m.poolsMu.RUnlock()

	var errs []error
	for _, root := range roots {
		n, perr := sweepStagingDir(filepath.Join(root, snapshotsSubdir, snapshotsStagingSubdir), maxAge)
		removed += n
		if perr != nil {
			errs = append(errs, fmt.Errorf("pool %s: %v", root, perr))
		}
	}
	return removed, errors.Join(errs...)
}

// sweepStagingDir removes regular files under stagingDir whose modtime is older
// than now-maxAge (maxAge == 0 removes all). Subdirs are skipped. An absent dir
// yields (0, nil). A per-file remove error is skipped (best-effort); a failure
// to read the directory itself is returned.
func sweepStagingDir(stagingDir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read snapshot staging dir: %v", err)
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if maxAge > 0 {
			info, ierr := e.Info()
			if ierr != nil || !info.ModTime().Before(cutoff) {
				continue
			}
		}
		if rerr := os.Remove(filepath.Join(stagingDir, e.Name())); rerr == nil {
			removed++
		}
	}
	return removed, nil
}
