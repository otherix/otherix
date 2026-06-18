// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

func seedOldBlob(t *testing.T, snapshotsDir string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(snapshotsDir, 0o750); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	sum := sha256.Sum256(content)
	d := hex.EncodeToString(sum[:])
	blob := filepath.Join(snapshotsDir, d+".qcow2")
	if err := os.WriteFile(blob, content, 0o644); err != nil {
		t.Fatalf("write old blob: %v", err)
	}
	if err := os.WriteFile(blob+".sha256", []byte(d), 0o644); err != nil {
		t.Fatalf("write old sidecar: %v", err)
	}
	return d
}

func TestRelocateMovesBlobAndRemovesOld(t *testing.T) {
	poolRoot := t.TempDir()
	snapshotsDir := filepath.Join(poolRoot, snapshotsSubdir)
	d := seedOldBlob(t, snapshotsDir, []byte("an-old-blob"))

	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	relocateSnapshotsToStore(discardLogger(), []string{poolRoot}, store)

	if !store.Has(d) {
		t.Fatalf("blob %s not relocated into store", d)
	}
	if _, err := os.Stat(filepath.Join(snapshotsDir, d+".qcow2")); !os.IsNotExist(err) {
		t.Errorf("old blob still present after relocation (want removed): %v", err)
	}
}

func TestRelocateLeavesCorruptOldIntact(t *testing.T) {
	poolRoot := t.TempDir()
	snapshotsDir := filepath.Join(poolRoot, snapshotsSubdir)
	// A blob whose sidecar claims a digest its content does not match (corrupt):
	// store.Put re-hashes and rejects, so the old copy must stay (fail-open).
	if err := os.MkdirAll(snapshotsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	claimed := "0000000000000000000000000000000000000000000000000000000000000000"
	blob := filepath.Join(snapshotsDir, claimed+".qcow2")
	if err := os.WriteFile(blob, []byte("not-matching-the-claimed-digest"), 0o644); err != nil {
		t.Fatalf("write corrupt blob: %v", err)
	}
	if err := os.WriteFile(blob+".sha256", []byte(claimed), 0o644); err != nil {
		t.Fatalf("write corrupt sidecar: %v", err)
	}

	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	relocateSnapshotsToStore(discardLogger(), []string{poolRoot}, store)

	if store.Has(claimed) {
		t.Errorf("corrupt blob made visible in store (want rejected)")
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("corrupt old blob removed (want left intact, fail-open): %v", err)
	}
}
