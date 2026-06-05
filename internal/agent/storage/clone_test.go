// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneImage_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "image.qcow2")
	dst := filepath.Join(dir, "vms", "abc", "disk.qcow2")

	want := []byte("pretend this is a qcow2 header and body")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	if err := CloneImage(src, dst); err != nil {
		t.Fatalf("CloneImage: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("dst contents mismatch: got %q, want %q", got, want)
	}

	// .tmp companion must not survive a successful clone.
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file %s should not exist after clone, stat=%v", dst+".tmp", err)
	}
}

func TestCloneImage_MissingSource(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out", "disk.qcow2")
	err := CloneImage(filepath.Join(dir, "missing.qcow2"), dst)
	if err == nil {
		t.Fatalf("CloneImage(missing) = nil, want error")
	}
}

func TestCloneImage_OverwriteExistingFinalIsAtomic(t *testing.T) {
	// If dst already exists, CloneImage writes to dst+".tmp" then renames,
	// so the final file is replaced atomically. Verify that a pre-existing
	// dst is replaced (not corrupted by interleaved writes).
	dir := t.TempDir()
	src := filepath.Join(dir, "image.qcow2")
	dst := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if err := os.WriteFile(dst, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	if err := CloneImage(src, dst); err != nil {
		t.Fatalf("CloneImage: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("dst = %q, want %q", got, "new")
	}
}
