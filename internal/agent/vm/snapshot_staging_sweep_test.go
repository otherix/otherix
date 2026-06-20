// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// stagingDirFor returns the snapshots/.staging dir under a pool root.
func stagingDirFor(root string) string {
	return filepath.Join(root, snapshotsSubdir, snapshotsStagingSubdir)
}

// plantStagingTemp writes a temp file into a pool's snapshots/.staging dir.
func plantStagingTemp(t *testing.T, root, name string) string {
	t.Helper()
	dir := stagingDirFor(root)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("temp"), 0o600); err != nil {
		t.Fatalf("write staging temp: %v", err)
	}
	return p
}

func TestSweepSnapshotStaging_BootRemovesAll(t *testing.T) {
	m := newTestManager(t)
	root := m.defaultTestPoolRoot(t)
	temp := plantStagingTemp(t, root, "backup-x-virtio0.qcow2")

	removed, err := m.SweepSnapshotStaging(0)
	if err != nil {
		t.Fatalf("SweepSnapshotStaging(0) error = %v, want nil", err)
	}
	if removed != 1 {
		t.Errorf("SweepSnapshotStaging(0) removed = %d, want 1", removed)
	}
	if _, statErr := os.Stat(temp); !os.IsNotExist(statErr) {
		t.Errorf("staging temp still present after boot sweep")
	}
}

func TestSweepSnapshotStaging_PeriodicSparesFreshRemovesOld(t *testing.T) {
	m := newTestManager(t)
	root := m.defaultTestPoolRoot(t)
	fresh := plantStagingTemp(t, root, "fresh.qcow2")
	old := plantStagingTemp(t, root, "old.qcow2")
	// Backdate the old temp well beyond the max age.
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	removed, err := m.SweepSnapshotStaging(24 * time.Hour)
	if err != nil {
		t.Fatalf("SweepSnapshotStaging(24h) error = %v, want nil", err)
	}
	if removed != 1 {
		t.Errorf("SweepSnapshotStaging(24h) removed = %d, want 1", removed)
	}
	if _, statErr := os.Stat(fresh); statErr != nil {
		t.Errorf("fresh staging temp removed by periodic sweep, want spared: %v", statErr)
	}
	if _, statErr := os.Stat(old); !os.IsNotExist(statErr) {
		t.Errorf("old staging temp still present after periodic sweep")
	}
}

func TestSweepSnapshotStaging_NeverTouchesDurableBlobs(t *testing.T) {
	m := newTestManager(t)
	root := m.defaultTestPoolRoot(t)
	plantStagingTemp(t, root, "backup-x.qcow2")

	// Plant a durable content-addressed blob + sidecar in snapshots/ proper.
	snapshotsDir := filepath.Join(root, snapshotsSubdir)
	blob := filepath.Join(snapshotsDir, "deadbeef.qcow2")
	sidecar := blob + ".sha256"
	if err := os.WriteFile(blob, []byte("blob"), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := os.WriteFile(sidecar, []byte("deadbeef"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	if _, err := m.SweepSnapshotStaging(0); err != nil {
		t.Fatalf("SweepSnapshotStaging(0) error = %v", err)
	}
	if _, statErr := os.Stat(blob); statErr != nil {
		t.Errorf("durable blob removed by staging sweep: %v", statErr)
	}
	if _, statErr := os.Stat(sidecar); statErr != nil {
		t.Errorf("durable blob sidecar removed by staging sweep: %v", statErr)
	}
}

func TestSweepSnapshotStaging_AbsentStagingDirIsNoOp(t *testing.T) {
	m := newTestManager(t)
	root := m.defaultTestPoolRoot(t)
	// Remove the staging dir AddPool created so it is genuinely absent.
	if err := os.RemoveAll(stagingDirFor(root)); err != nil {
		t.Fatalf("remove staging dir: %v", err)
	}

	removed, err := m.SweepSnapshotStaging(0)
	if err != nil {
		t.Errorf("SweepSnapshotStaging(0) on absent staging error = %v, want nil", err)
	}
	if removed != 0 {
		t.Errorf("SweepSnapshotStaging(0) on absent staging removed = %d, want 0", removed)
	}
}

func TestSweepSnapshotStaging_SkipsSubdirs(t *testing.T) {
	m := newTestManager(t)
	root := m.defaultTestPoolRoot(t)
	sub := filepath.Join(stagingDirFor(root), "subdir")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	removed, err := m.SweepSnapshotStaging(0)
	if err != nil {
		t.Fatalf("SweepSnapshotStaging(0) error = %v", err)
	}
	if removed != 0 {
		t.Errorf("SweepSnapshotStaging(0) removed = %d, want 0 (subdir skipped)", removed)
	}
	if _, statErr := os.Stat(sub); statErr != nil {
		t.Errorf("subdir removed by staging sweep: %v", statErr)
	}
}

func TestSweepSnapshotStaging_MultiPool(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool(%s): %v", poolName, err)
	}
	secondRoot := filepath.Join(t.TempDir(), "pool2")
	if err := m.AddPool("pool2", secondRoot); err != nil {
		t.Fatalf("AddPool(pool2): %v", err)
	}

	plantStagingTemp(t, poolRoot, "a.qcow2")
	plantStagingTemp(t, secondRoot, "b.qcow2")

	removed, err := m.SweepSnapshotStaging(0)
	if err != nil {
		t.Fatalf("SweepSnapshotStaging(0) error = %v, want nil", err)
	}
	if removed != 2 {
		t.Errorf("SweepSnapshotStaging(0) removed = %d, want 2 (both pools)", removed)
	}
}

func TestSweepSnapshotStaging_OnePoolErrorDoesNotAbortOther(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool(%s): %v", poolName, err)
	}
	secondRoot := filepath.Join(t.TempDir(), "pool2")
	if err := m.AddPool("pool2", secondRoot); err != nil {
		t.Fatalf("AddPool(pool2): %v", err)
	}

	// Healthy pool has a sweepable temp.
	plantStagingTemp(t, poolRoot, "good.qcow2")

	// Make the second pool's staging dir unreadable by replacing it with a
	// regular file: os.ReadDir then fails with ENOTDIR.
	badStaging := stagingDirFor(secondRoot)
	if err := os.RemoveAll(badStaging); err != nil {
		t.Fatalf("remove staging dir: %v", err)
	}
	if err := os.WriteFile(badStaging, []byte("x"), 0o600); err != nil {
		t.Fatalf("plant non-dir staging: %v", err)
	}

	removed, err := m.SweepSnapshotStaging(0)
	if err == nil {
		t.Errorf("SweepSnapshotStaging(0) error = nil, want surfaced per-pool error")
	}
	if removed != 1 {
		t.Errorf("SweepSnapshotStaging(0) removed = %d, want 1 (healthy pool swept despite other pool error)", removed)
	}
}
