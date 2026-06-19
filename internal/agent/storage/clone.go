// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package storage hosts agent-side storage operations: image cloning
// and (in future) image import / scan / delete on local pools. It
// currently covers only the full-copy clone path used by vm.create.
package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CloneImage makes a full copy of srcPath at dstPath. The copy is
// written to a "<dstPath>.tmp" sibling, fsynced, and renamed atomically
// so that interrupted clones never leave a partial qcow2 at the final
// path that a subsequent boot might pick up.
//
// The agent uses full-copy clones (cp image.qcow2 -> vm-disk.qcow2).
// Linked clones are deferred to a follow-up.
func CloneImage(srcPath, dstPath string) error {
	src, err := os.Open(srcPath) // #nosec G304 -- srcPath is a CP-issued image cache path, not user input.
	if err != nil {
		return fmt.Errorf("open image %s: %w", srcPath, err)
	}
	defer func() { _ = src.Close() }()

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dstPath), err)
	}

	tmpPath := dstPath + ".tmp"
	// #nosec G304 -- tmpPath is built from the agent's own pool root, not user input.
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open temp %s: %w", tmpPath, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy: %w", err)
	}

	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync: %w", err)
	}

	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, dstPath, err)
	}

	return nil
}
