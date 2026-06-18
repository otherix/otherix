// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// TestProduceBlobToStoreLandsInArtifactStore pins the C1 dual-write tail:
// produceBlobToStore converts a backup-target qcow2, verifies its magic + digest,
// and lands the blob (digest-keyed) in the dedicated per-node artifact store via
// store.Put. It reuses the package-level copyConvert (a verbatim qemu-img stand-in)
// and qcow2Body fixture so the capture path runs on darwin without qemu-img.
func TestProduceBlobToStoreLandsInArtifactStore(t *testing.T) {
	body := qcow2Body(0x42)
	src := filepath.Join(t.TempDir(), "backup.qcow2")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}

	blob, err := produceBlobToStore(context.Background(), src, store, copyConvert)
	if err != nil {
		t.Fatalf("produceBlobToStore: %v", err)
	}
	if blob.SHA256 != shaHex(body) {
		t.Errorf("blob.SHA256 = %q, want %q", blob.SHA256, shaHex(body))
	}
	if blob.SizeBytes != int64(len(body)) {
		t.Errorf("blob.SizeBytes = %d, want %d", blob.SizeBytes, len(body))
	}
	if !store.Has(blob.SHA256) {
		t.Fatalf("artifact store missing blob %s after produce", blob.SHA256)
	}
	rc, err := store.Get(blob.SHA256)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	_ = rc.Close()
}

// TestCaptureDualWritesToArtifactStore drives the REAL capture worker
// (CreateSnapshot -> runSnapshotCreate) with an artifact store wired in and
// asserts the C1 dual-write seam: the blob AND the manifest land in the store
// (keyed by the disk's blob digest), WHILE the slice-A disk-pool copy is still
// produced. Exercising the worker, not produceBlobToStore directly, is what
// proves the seam (the disk loop and the manifest write both fan out to the
// store).
func TestCaptureDualWritesToArtifactStore(t *testing.T) {
	m := newTestManager(t)
	m.diskCapturer = &fakeCapturer{}
	m.snapshotConvert = copyConvert

	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	m.artifactStore = store

	v := m.seedStoppedVMWithQcow2(t, "dualvm")
	task, err := m.CreateSnapshot(context.Background(), v.Name, SnapshotSpec{Name: "snap1"})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	done := m.awaitTask(t, task.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	wantSHA := shaHex(qcow2Body(0x11))

	// Slice-A disk-pool copy preserved.
	poolBlob := filepath.Join(m.defaultTestPoolRoot(t), "snapshots", wantSHA+".qcow2")
	if _, err := os.Stat(poolBlob); err != nil {
		t.Errorf("disk-pool blob missing at %q: %v (slice-A copy must be preserved)", poolBlob, err)
	}

	// C1 additive store copy: blob + manifest keyed by the blob digest.
	if !store.Has(wantSHA) {
		t.Errorf("artifact store missing blob %s after capture", wantSHA)
	}
	if _, err := store.ReadManifest(wantSHA); err != nil {
		t.Errorf("artifact store missing manifest keyed by %s: %v", wantSHA, err)
	}
}
