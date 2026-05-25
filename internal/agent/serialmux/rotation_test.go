// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotateLogFile_NoPriorRotation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	currentPath := filepath.Join(dir, logFileName)
	rotatedPath := filepath.Join(dir, rotatedFileName)

	if err := os.WriteFile(currentPath, []byte("old-data-1\n"), 0o600); err != nil {
		t.Fatalf("seed current: %v", err)
	}
	newFile, err := rotateLogFile(dir)
	if err != nil {
		t.Fatalf("rotateLogFile: %v", err)
	}
	defer func() { _ = newFile.Close() }()

	rotatedBytes, err := os.ReadFile(rotatedPath)
	if err != nil {
		t.Fatalf("read rotated: %v", err)
	}
	if !bytes.Equal(rotatedBytes, []byte("old-data-1\n")) {
		t.Errorf("rotated content = %q, want %q", rotatedBytes, "old-data-1\n")
	}

	currentBytes, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("read new current: %v", err)
	}
	if len(currentBytes) != 0 {
		t.Errorf("new current size = %d, want 0", len(currentBytes))
	}

	if _, err := newFile.Write([]byte("new-data\n")); err != nil {
		t.Fatalf("write through returned handle: %v", err)
	}
	if err := newFile.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatalf("re-read current: %v", err)
	}
	if !bytes.Equal(after, []byte("new-data\n")) {
		t.Errorf("post-write current = %q, want %q", after, "new-data\n")
	}
}

func TestRotateLogFile_OverwritesExistingRotated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	currentPath := filepath.Join(dir, logFileName)
	rotatedPath := filepath.Join(dir, rotatedFileName)

	if err := os.WriteFile(rotatedPath, []byte("oldest-keep-out\n"), 0o600); err != nil {
		t.Fatalf("seed rotated: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte("middle-batch\n"), 0o600); err != nil {
		t.Fatalf("seed current: %v", err)
	}

	newFile, err := rotateLogFile(dir)
	if err != nil {
		t.Fatalf("rotateLogFile: %v", err)
	}
	defer func() { _ = newFile.Close() }()

	rotatedBytes, err := os.ReadFile(rotatedPath)
	if err != nil {
		t.Fatalf("read rotated: %v", err)
	}
	if !bytes.Equal(rotatedBytes, []byte("middle-batch\n")) {
		t.Errorf("rotated content = %q, want previous-current %q", rotatedBytes, "middle-batch\n")
	}
}

// TestRotateLogFile_CurrentAbsentIsNotFatal covers the surprising-but-
// recoverable case where the current log file has gone missing before
// rotation fires (e.g. an operator manually removed it). Rotation
// still creates a fresh current file and returns its handle.
func TestRotateLogFile_CurrentAbsentIsNotFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	currentPath := filepath.Join(dir, logFileName)

	newFile, err := rotateLogFile(dir)
	if err != nil {
		t.Fatalf("rotateLogFile: %v", err)
	}
	defer func() { _ = newFile.Close() }()

	if _, err := os.Stat(currentPath); err != nil {
		t.Errorf("new current not present: %v", err)
	}
}
