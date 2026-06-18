// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentapi"
)

// fakeCapturer is a diskCapturer test double: instead of dialing QMP and
// running qemu blockdev-backup, it copies the VM's source disk verbatim to
// dest (a standalone qcow2 the produceBlob seam then hashes). It records the
// number of Capture calls so the (vm, snapshot_name) idempotency guard can be
// asserted (a redelivered create must NOT trigger a second capture).
type fakeCapturer struct {
	mu    sync.Mutex
	calls int32
	// srcOverride, when set for a device, is copied instead of v.DiskPath.
	srcOverride map[string]string
}

func (f *fakeCapturer) Capture(_ context.Context, _ *VM, device, src, dest string) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	from := src
	if f.srcOverride != nil {
		if o, ok := f.srcOverride[device]; ok {
			from = o
		}
	}
	f.mu.Unlock()
	b, err := os.ReadFile(from) //nolint:gosec // test-local path
	if err != nil {
		return err
	}
	return os.WriteFile(dest, b, 0o600)
}

func (f *fakeCapturer) callCount() int32 { return atomic.LoadInt32(&f.calls) }

// seedStoppedVMWithQcow2 seeds a stopped VM whose boot disk carries the qcow2
// magic so produceBlob's validateQcow2Magic passes when the fake capturer
// copies it verbatim.
func (m *Manager) seedStoppedVMWithQcow2(t *testing.T, name string) *VM {
	t.Helper()
	v := m.seedStoppedVM(t, name)
	if err := os.WriteFile(v.DiskPath, qcow2Body(0x11), 0o600); err != nil {
		t.Fatalf("write qcow2 boot disk: %v", err)
	}
	return v
}

// awaitTask polls the task store until the task reaches a terminal status or
// the deadline elapses.
func (m *Manager) awaitTask(t *testing.T, id [16]byte) *AgentTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task := m.tasks.Get(id)
		if task != nil && (task.Status == TaskStatusSuccess || task.Status == TaskStatusFailed) {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %x did not reach terminal status in time", id)
	return nil
}

func TestCreateSnapshot_ReturnsTaskAndManifest(t *testing.T) {
	m := newTestManager(t)
	cap := &fakeCapturer{}
	m.diskCapturer = cap
	m.snapshotConvert = copyConvert

	v := m.seedStoppedVMWithQcow2(t, "snapvm")

	task, err := m.CreateSnapshot(context.Background(), v.Name, SnapshotSpec{Name: "snap1"})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if task == nil {
		t.Fatal("CreateSnapshot returned nil task")
	}

	done := m.awaitTask(t, task.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	var snap agentapi.Snapshot
	if err := json.Unmarshal(done.Result, &snap); err != nil {
		t.Fatalf("decode result as agentapi.Snapshot: %v (raw=%s)", err, done.Result)
	}
	if snap.Name != "snap1" {
		t.Errorf("snap.Name = %q, want snap1", snap.Name)
	}
	if snap.VMUUID != v.ID {
		t.Errorf("snap.VMUUID = %v, want %v", snap.VMUUID, v.ID)
	}
	if snap.VMStateAtSnapshot != agentapi.SnapshotVMStateAtSnapshotStopped {
		t.Errorf("vm_state_at_snapshot = %q, want stopped", snap.VMStateAtSnapshot)
	}
	if len(snap.Disks) != 1 {
		t.Fatalf("len(disks) = %d, want 1", len(snap.Disks))
	}
	d0 := snap.Disks[0]
	if d0.Index != 0 || d0.Device != "virtio0" {
		t.Errorf("disk[0] = (index=%d, device=%q), want (0, virtio0)", d0.Index, d0.Device)
	}
	if d0.Format != agentapi.SnapshotDisksFormatQcow2 {
		t.Errorf("disk[0].Format = %q, want qcow2", d0.Format)
	}
	wantSHA := shaHex(qcow2Body(0x11))
	if d0.Sha256 != wantSHA {
		t.Errorf("disk[0].Sha256 = %q, want %q", d0.Sha256, wantSHA)
	}

	// Manifest JSON landed under snapshots/manifests/.
	root := m.defaultTestPoolRoot(t)
	manPath := filepath.Join(root, "snapshots", "manifests", "snap1.json")
	if _, err := os.Stat(manPath); err != nil {
		t.Errorf("manifest not written at %q: %v", manPath, err)
	}

	// ListSnapshots surfaces the blob.
	blobs, err := ListSnapshots(filepath.Join(root, "snapshots"))
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	found := false
	for _, b := range blobs {
		if b.SHA256 == wantSHA {
			found = true
		}
	}
	if !found {
		t.Errorf("ListSnapshots did not surface blob %q; got %+v", wantSHA, blobs)
	}

	if got := cap.callCount(); got != 1 {
		t.Errorf("capturer call count = %d, want 1", got)
	}
}

func TestCreateSnapshot_WithMemory_Rejected(t *testing.T) {
	m := newTestManager(t)
	m.diskCapturer = &fakeCapturer{}
	m.snapshotConvert = copyConvert
	v := m.seedStoppedVMWithQcow2(t, "memvm")

	withMem := true
	_, err := m.CreateSnapshot(context.Background(), v.Name, SnapshotSpec{Name: "snap1", WithMemory: withMem})
	if err == nil {
		t.Fatal("CreateSnapshot(with_memory=true) returned nil error, want rejection")
	}
}

// writeManifestOnDisk writes a snapshot manifest plus a content-addressed
// blob (+ sidecar) file for each referenced digest under the pool's
// snapshots/ layout, mirroring exactly what a settled CreateSnapshot leaves
// behind. It is the lower-friction way to construct a precise
// shared-vs-unique blob-reference state that drives the REAL DeleteSnapshot
// survivor scan, instead of orchestrating multiple capture runs.
func (m *Manager) writeManifestOnDisk(t *testing.T, poolRoot string, v *VM, name string, digests ...string) {
	t.Helper()
	disks := make([]snapshotManifestDisk, 0, len(digests))
	for i, sha := range digests {
		disks = append(disks, snapshotManifestDisk{
			Index:     i,
			Device:    fmt.Sprintf("virtio%d", i),
			SHA256:    sha,
			SizeBytes: 8,
			Format:    "qcow2",
		})
		snapshotsDir := filepath.Join(poolRoot, "snapshots")
		if err := os.MkdirAll(snapshotsDir, 0o750); err != nil {
			t.Fatalf("mkdir snapshots dir: %v", err)
		}
		blobPath := filepath.Join(snapshotsDir, sha+".qcow2")
		if err := os.WriteFile(blobPath, qcow2Body(0x22), 0o600); err != nil {
			t.Fatalf("write blob %s: %v", sha, err)
		}
		if err := os.WriteFile(blobPath+".sha256", []byte(sha), 0o644); err != nil {
			t.Fatalf("write sidecar %s: %v", sha, err)
		}
	}
	man := snapshotManifest{
		Name:              name,
		VMUUID:            v.ID,
		VMName:            v.Name,
		Architecture:      string(v.Architecture),
		VMStateAtSnapshot: "stopped",
		CreatedAt:         time.Now().UTC(),
		Disks:             disks,
	}
	if err := writeSnapshotManifest(poolRoot, man); err != nil {
		t.Fatalf("write manifest %s: %v", name, err)
	}
}

// blobExists reports whether the content-addressed blob (and its sidecar) for
// digest sha is present under the pool's snapshots/ dir.
func blobExists(poolRoot, sha string) bool {
	blobPath := filepath.Join(poolRoot, "snapshots", sha+".qcow2")
	if _, err := os.Stat(blobPath); err != nil {
		return false
	}
	return true
}

// manifestExists reports whether the named snapshot's manifest is present.
func manifestExists(poolRoot, name string) bool {
	if _, err := os.Stat(snapshotManifestPath(poolRoot, name)); err != nil {
		return false
	}
	return true
}

// TestDeleteSnapshot_KeepsBlobSharedByAnotherManifest pins the fail-closed
// blob GC: deleting snapshot A removes A's manifest and A's UNIQUE blob, but
// leaves the SHARED blob (still referenced by surviving snapshot B) and all of
// B's state intact. Breaking the survivor scan in production (deleting all of
// A's blobs unconditionally) MUST fail this test.
func TestDeleteSnapshot_KeepsBlobSharedByAnotherManifest(t *testing.T) {
	m := newTestManager(t)
	v := m.seedStoppedVM(t, "snapvm")
	root := m.defaultTestPoolRoot(t)

	shared := shaHex([]byte("shared-blob-content"))
	uniqueA := shaHex([]byte("unique-A-content"))
	uniqueB := shaHex([]byte("unique-B-content"))

	m.writeManifestOnDisk(t, root, v, "snapA", shared, uniqueA)
	m.writeManifestOnDisk(t, root, v, "snapB", shared, uniqueB)

	task, err := m.DeleteSnapshot(context.Background(), v.Name, "snapA")
	if err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	done := m.awaitTask(t, task.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("delete task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	if manifestExists(root, "snapA") {
		t.Error("snapA manifest still present after delete, want removed")
	}
	if blobExists(root, uniqueA) {
		t.Error("snapA unique blob still present, want removed (no surviving reference)")
	}
	if !blobExists(root, shared) {
		t.Error("shared blob removed, want KEPT (snapB still references it)")
	}
	if !manifestExists(root, "snapB") {
		t.Error("snapB manifest removed, want intact")
	}
	if !blobExists(root, uniqueB) {
		t.Error("snapB unique blob removed, want intact")
	}
}

// TestDeleteSnapshot_EnumerationError_LeaksNothingDeleted pins the fail-closed
// leak contract: when the manifest layer errors (here: the manifests dir is
// chmod 0000, so both the target read and the survivor enumeration fail with a
// non-fs.ErrNotExist error), DeleteSnapshot must delete ZERO blobs - leak
// rather than risk removing one still in use. runSnapshotDelete reads the
// target manifest before it ever reaches the blob-removal loop, so any
// manifest-layer error short-circuits before a single os.Remove of a blob; the
// assertion below proves the orphan blob survives. We trigger the error at the
// manifests dir (the cleanest deterministic seam-free route); the chmod-0000
// approach is skipped on filesystems / uids that do not enforce it.
func TestDeleteSnapshot_EnumerationError_LeaksNothingDeleted(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("chmod 0000 does not deny root; this trigger needs an unprivileged uid")
	}
	m := newTestManager(t)
	v := m.seedStoppedVM(t, "leakvm")
	root := m.defaultTestPoolRoot(t)

	orphan := shaHex([]byte("orphan-blob-content"))
	m.writeManifestOnDisk(t, root, v, "snapA", orphan)

	manifestsDir := filepath.Join(root, "snapshots", "manifests")
	if err := os.Chmod(manifestsDir, 0o000); err != nil {
		t.Fatalf("chmod manifests dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(manifestsDir, 0o750) })
	// Probe whether 0000 actually denies access on this fs; if not, skip.
	if _, err := os.ReadDir(manifestsDir); err == nil {
		t.Skip("filesystem does not enforce chmod 0000 on directory reads; cannot trigger the error")
	}

	task, err := m.DeleteSnapshot(context.Background(), v.Name, "snapA")
	if err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	_ = m.awaitTask(t, task.ID)

	// Restore access so we can inspect the blob, then assert it was NOT removed.
	if err := os.Chmod(manifestsDir, 0o750); err != nil {
		t.Fatalf("restore manifests dir perms: %v", err)
	}
	if !blobExists(root, orphan) {
		t.Error("blob removed on manifest-layer error, want LEAKED (fail-closed: delete nothing)")
	}
}

// TestCreateSnapshot_MultiDisk_ManifestOrderedByIndex pins the manifest's
// deterministic disk ordering: a VM yielding two disks in REVERSE discovery
// order (virtio1 before virtio0) must produce a manifest whose disks are sorted
// ascending by index [(0,virtio0),(1,virtio1)]. Removing runSnapshotCreate's
// sort.Slice(...byIndex) MUST fail this test. Slice A's enumerator only yields
// a single boot disk, so the two-disk fixture is injected through the
// snapshotDiskDevices seam.
func TestCreateSnapshot_MultiDisk_ManifestOrderedByIndex(t *testing.T) {
	m := newTestManager(t)
	cap := &fakeCapturer{}
	m.diskCapturer = cap
	m.snapshotConvert = copyConvert

	v := m.seedStoppedVMWithQcow2(t, "multidiskvm")

	// A second disk with distinct content so its blob hashes distinctly.
	disk1 := filepath.Join(filepath.Dir(v.DiskPath), "disk1.qcow2")
	if err := os.WriteFile(disk1, qcow2Body(0x33), 0o600); err != nil {
		t.Fatalf("write second disk: %v", err)
	}
	// Capturer copies the right source per device.
	cap.srcOverride = map[string]string{"virtio0": v.DiskPath, "virtio1": disk1}

	// Inject a two-disk enumerator that returns them in REVERSE index order, so
	// only the production sort can put the manifest back in ascending order.
	m.snapshotDiskDevices = func(vm *VM) []snapshotDiskDevice {
		return []snapshotDiskDevice{
			{index: 1, device: "virtio1", src: disk1},
			{index: 0, device: "virtio0", src: vm.DiskPath},
		}
	}

	task, err := m.CreateSnapshot(context.Background(), v.Name, SnapshotSpec{Name: "multi"})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	done := m.awaitTask(t, task.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	var snap agentapi.Snapshot
	if err := json.Unmarshal(done.Result, &snap); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, done.Result)
	}
	if len(snap.Disks) != 2 {
		t.Fatalf("len(disks) = %d, want 2", len(snap.Disks))
	}
	if snap.Disks[0].Index != 0 || snap.Disks[0].Device != "virtio0" {
		t.Errorf("disk[0] = (index=%d, device=%q), want (0, virtio0)", snap.Disks[0].Index, snap.Disks[0].Device)
	}
	if snap.Disks[1].Index != 1 || snap.Disks[1].Device != "virtio1" {
		t.Errorf("disk[1] = (index=%d, device=%q), want (1, virtio1)", snap.Disks[1].Index, snap.Disks[1].Device)
	}
}

func TestCreateSnapshot_Idempotent_SameVMAndName(t *testing.T) {
	m := newTestManager(t)
	cap := &fakeCapturer{}
	m.diskCapturer = cap
	m.snapshotConvert = copyConvert
	v := m.seedStoppedVMWithQcow2(t, "idemvm")

	first, err := m.CreateSnapshot(context.Background(), v.Name, SnapshotSpec{Name: "snap1"})
	if err != nil {
		t.Fatalf("first CreateSnapshot: %v", err)
	}
	done := m.awaitTask(t, first.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("first task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	// Second create with the same (vm, name): manifest already exists, so it
	// must NOT start a second capture. The capturer call count stays at 1.
	second, err := m.CreateSnapshot(context.Background(), v.Name, SnapshotSpec{Name: "snap1"})
	if err != nil {
		t.Fatalf("second CreateSnapshot: %v", err)
	}
	_ = m.awaitTask(t, second.ID)

	if got := cap.callCount(); got != 1 {
		t.Errorf("capturer call count after redelivery = %d, want 1 (no second capture)", got)
	}
}
