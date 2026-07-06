// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package state

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// ScanState walks stateDir and reads meta.json from every direct
// subdirectory whose name parses as a UUID. Subdirectories without a
// meta.json (or whose meta.json fails to decode) are skipped with a
// warning log so a single corrupted entry does not abort startup.
//
// It also returns the number of VM directories that were SKIPPED (a
// UUID-named dir present but its meta.json missing or unreadable). A
// non-zero skip count means the replayed VM set is INCOMPLETE, which a
// caller driving a destructive reconcile off "not in the replayed set"
// (the orphan-tap sweep) MUST treat as unsafe: a skipped dir may be a
// live VM whose host devices are still up but whose NICs cannot be
// re-derived from the unreadable meta, so acting on the partial set would
// sever a running VM. Fail toward inaction on a non-zero count.
//
// Returned slice ordering is filesystem-dependent (effectively
// readdir order) — callers must not rely on it.
func ScanState(stateDir string, log *slog.Logger) ([]*VMMeta, int, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, 0, fmt.Errorf("mkdir %s: %w", stateDir, err)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, 0, fmt.Errorf("read state dir %s: %w", stateDir, err)
	}

	var (
		metas   []*VMMeta
		skipped int
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := uuid.Parse(e.Name()); err != nil {
			// Defensive: ignore non-UUID directories (e.g. stray operator
			// artifacts) rather than treat them as VMs. Not counted as a skip:
			// a non-UUID dir was never a VM, so it cannot own a VM tap.
			continue
		}

		vmDir := filepath.Join(stateDir, e.Name())
		meta, err := ReadMeta(vmDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				log.Warn("vm dir without meta.json — skipping", "vm_dir", vmDir)
				skipped++
				continue
			}
			log.Warn("failed to read meta.json — skipping", "vm_dir", vmDir, "err", err)
			skipped++
			continue
		}
		metas = append(metas, meta)
	}
	return metas, skipped, nil
}
