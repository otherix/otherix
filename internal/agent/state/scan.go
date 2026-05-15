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
// Returned slice ordering is filesystem-dependent (effectively
// readdir order) — callers must not rely on it.
func ScanState(stateDir string, log *slog.Logger) ([]*VMMeta, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", stateDir, err)
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("read state dir %s: %w", stateDir, err)
	}

	var metas []*VMMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := uuid.Parse(e.Name()); err != nil {
			// Defensive: ignore non-UUID directories (e.g. stray operator
			// artifacts) rather than treat them as VMs.
			continue
		}

		vmDir := filepath.Join(stateDir, e.Name())
		meta, err := ReadMeta(vmDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				log.Warn("vm dir without meta.json — skipping", "vm_dir", vmDir)
				continue
			}
			log.Warn("failed to read meta.json — skipping", "vm_dir", vmDir, "err", err)
			continue
		}
		metas = append(metas, meta)
	}
	return metas, nil
}
