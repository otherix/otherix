// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// putStoreBlob lands a content-addressed qcow2 blob in the artifact store (NOT
// the disk-pool snapshots dir) and returns its digest + body, so a dual-read
// test can prove materialiseFromSnapshot reads the store when the disk pool has
// no copy (the post-C1-pull placement: the broker lands the blob in the store).
func putStoreBlob(t *testing.T, store *artifactstore.Store, tag byte) (sha string, body []byte) {
	t.Helper()
	body = qcow2Body(tag)
	sha = shaHex(body)
	if err := store.Put(sha, bytes.NewReader(body)); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	return sha, body
}

// snapshotVMFixture seeds a pending VM in the default pool with the per-VM disk
// paths set (but no disk file), so materialiseFromSnapshot has a concrete
// v.DiskPath to clone the blob into.
func (m *Manager) snapshotVMFixture(t *testing.T, poolName, poolRoot string) *VM {
	t.Helper()
	vmID := uuid.New()
	disk, qmp, console, pid := m.vmPaths(poolRoot, vmID)
	if err := os.MkdirAll(filepath.Dir(disk), 0o750); err != nil {
		t.Fatalf("mkdir vm dir: %v", err)
	}
	v := &VM{
		ID:            vmID,
		Name:          "snap-restored",
		PoolName:      poolName,
		Status:        StatusPending,
		DiskPath:      disk,
		QMPSocket:     qmp,
		ConsoleSocket: console,
		PIDFile:       pid,
	}
	m.mu.Lock()
	m.vms[vmID] = v
	m.mu.Unlock()
	return v
}

// TestMaterialiseFromSnapshot_ReadsArtifactStoreFirst pins the C1 agent dual-read
// seam: when the snapshot blob lives ONLY in the dedicated artifact store (where
// a C1 cross-node pull lands it) and NOT in the disk-pool snapshots dir,
// materialiseFromSnapshot must find it in the store and clone the boot disk from
// it. Without this dual-read the CP pull lands the blob where the agent does not
// look and recreate fails snapshot_blob_missing.
func TestMaterialiseFromSnapshot_ReadsArtifactStoreFirst(t *testing.T) {
	m := newTestManager(t)
	poolName := m.defaultTestPool()
	poolRoot := m.defaultTestPoolRoot(t)

	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	m.artifactStore = store
	sha, body := putStoreBlob(t, store, 0x42)

	// Sanity: the blob is NOT in the disk-pool snapshots dir (store-only).
	poolBlob := filepath.Join(poolRoot, snapshotsSubdir, sha+".qcow2")
	if _, err := os.Stat(poolBlob); err == nil {
		t.Fatalf("disk-pool blob unexpectedly present at %q; the test must seed store-only", poolBlob)
	}

	v := m.snapshotVMFixture(t, poolName, poolRoot)
	ref := &SnapshotRef{Pool: poolName, Disks: []SnapshotDiskRef{{Index: 0, Device: "virtio0", SHA256: sha}}}

	if code, msg := m.materialiseFromSnapshot(v, ref); code != "" {
		t.Fatalf("materialiseFromSnapshot = (%q, %q), want success; the store-resident blob must be read", code, msg)
	}

	got, err := os.ReadFile(v.DiskPath) //nolint:gosec // test-local path
	if err != nil {
		t.Fatalf("read recreated disk: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("recreated disk = %d bytes, want clone of the %d-byte store blob", len(got), len(body))
	}
}

// TestMaterialiseFromSnapshot_FallsBackToDiskPool pins the pre-relocation
// path: when the blob lives ONLY in the disk-pool snapshots dir (no store copy,
// or no store configured), materialiseFromSnapshot still finds and clones it.
func TestMaterialiseFromSnapshot_FallsBackToDiskPool(t *testing.T) {
	m := newTestManager(t)
	poolName := m.defaultTestPool()
	poolRoot := m.defaultTestPoolRoot(t)

	// Store wired but EMPTY: the blob is only in the disk pool, so the read must
	// fall back to {poolRoot}/snapshots/{sha}.qcow2.
	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	m.artifactStore = store

	sha, body := writeBlob(t, poolRoot, 0x77)
	if store.Has(sha) {
		t.Fatalf("store unexpectedly holds %s; the fallback test must seed disk-pool-only", sha)
	}

	v := m.snapshotVMFixture(t, poolName, poolRoot)
	ref := &SnapshotRef{Pool: poolName, Disks: []SnapshotDiskRef{{Index: 0, Device: "virtio0", SHA256: sha}}}

	if code, msg := m.materialiseFromSnapshot(v, ref); code != "" {
		t.Fatalf("materialiseFromSnapshot = (%q, %q), want success via disk-pool fallback", code, msg)
	}

	got, err := os.ReadFile(v.DiskPath) //nolint:gosec // test-local path
	if err != nil {
		t.Fatalf("read recreated disk: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("recreated disk = %d bytes, want clone of the %d-byte disk-pool blob", len(got), len(body))
	}
}

// TestMaterialiseFromSnapshot_MissingEverywhere confirms a blob absent from BOTH
// the store and the disk pool still fails closed with snapshot_blob_missing.
func TestMaterialiseFromSnapshot_MissingEverywhere(t *testing.T) {
	m := newTestManager(t)
	poolName := m.defaultTestPool()
	poolRoot := m.defaultTestPoolRoot(t)

	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	m.artifactStore = store

	v := m.snapshotVMFixture(t, poolName, poolRoot)
	sha := shaHex(qcow2Body(0x99))
	ref := &SnapshotRef{Pool: poolName, Disks: []SnapshotDiskRef{{Index: 0, Device: "virtio0", SHA256: sha}}}

	code, _ := m.materialiseFromSnapshot(v, ref)
	if code != "snapshot_blob_missing" {
		t.Errorf("materialiseFromSnapshot code = %q, want snapshot_blob_missing when the blob is absent everywhere", code)
	}
}
