// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// cidataBuildSpy records one cidata-build invocation so the recreate test can
// assert the always-on seed carries the new VM's name as hostname.
type cidataBuildSpy struct {
	mu       sync.Mutex
	calls    int
	hostname string
	path     string
}

func (s *cidataBuildSpy) build(path, hostname string, _, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.hostname = hostname
	s.path = path
	// Materialize a real (non-empty) file so callers that stat the seed see it.
	return os.WriteFile(path, []byte("cidata-stub"), 0o600)
}

// writeBlob writes a content-addressed snapshot blob (qcow2-magic body) under
// {poolRoot}/snapshots/{sha}.qcow2 + sidecar and returns its digest. The body
// is deterministic per tag so the test can compare the recreated disk's bytes.
func writeBlob(t *testing.T, poolRoot string, tag byte) (sha string, body []byte) {
	t.Helper()
	body = qcow2Body(tag)
	dir := filepath.Join(poolRoot, snapshotsSubdir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	sum := sha256.Sum256(body)
	sha = hex.EncodeToString(sum[:])
	blobPath := filepath.Join(dir, sha+".qcow2")
	if err := os.WriteFile(blobPath, body, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := os.WriteFile(blobPath+".sha256", []byte(sha), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return sha, body
}

// TestRunCreate_FromSnapshot_MaterializesDisks drives the recreate branch of
// runCreate: with CreateSpec.SourceSnapshot set, the boot disk is cloned from
// the content-addressed snapshot blob (no image download), and the always-on
// cidata seed is built with the new VM name as hostname. runCreate fails at the
// qemu spawn step (no real guest in unit tests) AFTER the disk + seed are
// materialized, which is exactly what this test asserts.
func TestRunCreate_FromSnapshot_MaterializesDisks(t *testing.T) {
	m := newTestManager(t)
	spy := &cidataBuildSpy{}
	m.createBuildCidata = spy.build

	poolName := m.defaultTestPool()
	poolRoot := m.defaultTestPoolRoot(t)
	sha, body := writeBlob(t, poolRoot, 0x42)

	vmID := uuid.New()
	const vmName = "snap-restored"
	disk, qmp, console, pid := m.vmPaths(poolRoot, vmID)
	v := &VM{
		ID:            vmID,
		Name:          vmName,
		VCPUs:         1,
		MemoryMib:     512,
		PoolName:      poolName,
		Status:        StatusPending,
		DiskPath:      disk,
		QMPSocket:     qmp,
		ConsoleSocket: console,
		PIDFile:       pid,
		CidataPath:    filepath.Join(m.stateDir, vmID.String(), "cidata.iso"),
	}
	m.mu.Lock()
	m.vms[vmID] = v
	m.mu.Unlock()
	task := m.tasks.Create(TaskKindVMCreate, vmID)

	spec := CreateSpec{
		UUID:      vmID,
		Name:      vmName,
		VCPUs:     1,
		MemoryMib: 512,
		PoolName:  poolName,
		Format:    "qcow2",
		SourceSnapshot: &SnapshotRef{
			Pool: poolName,
			Disks: []SnapshotDiskRef{
				{Index: 0, Device: "virtio0", SHA256: sha},
			},
		},
	}

	m.runCreate(task.ID, v, spec)

	// The boot disk must be a clone of the blob (content matches).
	got, err := os.ReadFile(disk) //nolint:gosec // test-local path
	if err != nil {
		t.Fatalf("read recreated disk: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("recreated disk content = %d bytes, want clone of %d-byte blob", len(got), len(body))
	}

	// The always-on cidata seed was built with the new VM name as hostname.
	if spy.calls != 1 {
		t.Fatalf("cidata builds = %d, want 1", spy.calls)
	}
	if spy.hostname != vmName {
		t.Errorf("cidata hostname = %q, want %q", spy.hostname, vmName)
	}
	if _, err := os.Stat(v.CidataPath); err != nil {
		t.Errorf("cidata seed not written at %s: %v", v.CidataPath, err)
	}

	// No image download happened: the image cache stays empty (recreate clones
	// from the snapshot blob, never EnsureImageInto).
	if entries, err := os.ReadDir(filepath.Join(poolRoot, "images")); err == nil && len(entries) > 0 {
		t.Errorf("recreate populated the image cache (%d entries); expected snapshot-only materialize", len(entries))
	}
}
