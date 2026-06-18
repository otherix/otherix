// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/qemu"
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

	// Manifest JSON landed under snapshots/manifests/, VM-scoped by uuid.
	root := m.defaultTestPoolRoot(t)
	manPath := snapshotManifestPath(root, v.ID, "snap1")
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

// TestCreateSnapshot_SameNameDifferentVMs_NoCollision pins the real-agent bug
// the recreate smoke caught: snapshot names are unique only per-VM, so two VMs
// snapshotting with the same name "s1" must produce two INDEPENDENT manifests
// and blobs - the second must NOT hit the (vm, name) idempotency of the first
// and return its stale blob. Manifests are VM-scoped ({vm_uuid}__{name}.json).
func TestCreateSnapshot_SameNameDifferentVMs_NoCollision(t *testing.T) {
	m := newTestManager(t)
	m.diskCapturer = &fakeCapturer{}
	m.snapshotConvert = copyConvert
	root := m.defaultTestPoolRoot(t)

	vA := m.seedStoppedVM(t, "vm-a")
	if err := os.WriteFile(vA.DiskPath, qcow2Body(0xA1), 0o600); err != nil {
		t.Fatalf("write vm-a disk: %v", err)
	}
	vB := m.seedStoppedVM(t, "vm-b")
	if err := os.WriteFile(vB.DiskPath, qcow2Body(0xB2), 0o600); err != nil {
		t.Fatalf("write vm-b disk: %v", err)
	}
	shaA := shaHex(qcow2Body(0xA1))
	shaB := shaHex(qcow2Body(0xB2))

	for _, vm := range []*VM{vA, vB} {
		task, err := m.CreateSnapshot(context.Background(), vm.Name, SnapshotSpec{Name: "s1"})
		if err != nil {
			t.Fatalf("CreateSnapshot(%s): %v", vm.Name, err)
		}
		if done := m.awaitTask(t, task.ID); done.Status != TaskStatusSuccess {
			t.Fatalf("%s snapshot status = %q, want success (err=%+v)", vm.Name, done.Status, done.Error)
		}
	}

	// Both manifests exist independently (no name-only collision).
	if !manifestExists(root, vA.ID, "s1") || !manifestExists(root, vB.ID, "s1") {
		t.Fatal("expected an independent s1 manifest for each VM")
	}
	// Each VM's snapshot references its OWN disk's blob, not the other's.
	for _, tc := range []struct {
		vm      *VM
		wantSHA string
	}{{vA, shaA}, {vB, shaB}} {
		snap, ok, err := m.VMSnapshot(tc.vm.Name, "s1")
		if err != nil || !ok {
			t.Fatalf("VMSnapshot(%s, s1) ok=%v err=%v", tc.vm.Name, ok, err)
		}
		if len(snap.Disks) != 1 || snap.Disks[0].Sha256 != tc.wantSHA {
			t.Errorf("%s s1 disk sha = %v, want %q (got someone else's blob = collision)", tc.vm.Name, snap.Disks, tc.wantSHA)
		}
	}
	if shaA == shaB {
		t.Fatal("test setup: vm-a and vm-b disks must differ")
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

// manifestExists reports whether the named snapshot's manifest is present for
// the given VM (manifests are VM-scoped: {vm_uuid}__{name}.json).
func manifestExists(poolRoot string, vmUUID uuid.UUID, name string) bool {
	if _, err := os.Stat(snapshotManifestPath(poolRoot, vmUUID, name)); err != nil {
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

	// The CP refgraph orphans only A's unique blob (shared still referenced by B).
	task, err := m.DeleteSnapshotByID(context.Background(), v.ID, "snapA", []string{uniqueA})
	if err != nil {
		t.Fatalf("DeleteSnapshotByID: %v", err)
	}
	done := m.awaitTask(t, task.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("delete task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	if manifestExists(root, v.ID, "snapA") {
		t.Error("snapA manifest still present after delete, want removed")
	}
	if blobExists(root, uniqueA) {
		t.Error("snapA unique blob still present, want removed (no surviving reference)")
	}
	if !blobExists(root, shared) {
		t.Error("shared blob removed, want KEPT (snapB still references it)")
	}
	if !manifestExists(root, v.ID, "snapB") {
		t.Error("snapB manifest removed, want intact")
	}
	if !blobExists(root, uniqueB) {
		t.Error("snapB unique blob removed, want intact")
	}
}

// TestDeleteSnapshotByID_VMUnregistered_StillDeletes is the agent-side regression
// for the standalone-snapshot promise: a snapshot whose source VM is NOT registered
// on the agent (the VM was deleted) must still have its manifest + orphaned blob
// removed. DeleteSnapshotByID locates the manifest by vm_uuid across the registered
// pools and removes the content-addressed blob by digest, never calling ByName. A
// delete path keyed on the live VM (the bug) cannot serve this and would leave the
// blob leaked forever.
func TestDeleteSnapshotByID_VMUnregistered_StillDeletes(t *testing.T) {
	m := newTestManager(t)
	root := m.defaultTestPoolRoot(t)

	// A "ghost" VM: a manifest + blob exist on disk, but the VM is NOT registered
	// (m.ByName(ghost.Name) would fail) - exactly the post-VM-delete state.
	ghost := &VM{ID: uuid.New(), Name: "ghost", Architecture: qemu.HostArch()}
	if _, err := m.ByName(ghost.Name); err == nil {
		t.Fatal("test setup: ghost VM must NOT be registered")
	}
	orphan := shaHex([]byte("ghost-orphan-content"))
	m.writeManifestOnDisk(t, root, ghost, "snapG", orphan)

	task, err := m.DeleteSnapshotByID(context.Background(), ghost.ID, "snapG", []string{orphan})
	if err != nil {
		t.Fatalf("DeleteSnapshotByID: %v", err)
	}
	done := m.awaitTask(t, task.ID)
	if done.Status != TaskStatusSuccess {
		t.Fatalf("delete task status = %q, want success (err=%+v)", done.Status, done.Error)
	}

	if manifestExists(root, ghost.ID, "snapG") {
		t.Error("ghost snapshot manifest still present after delete, want removed (VM-independent)")
	}
	if blobExists(root, orphan) {
		t.Error("ghost orphan blob still present after delete, want removed (content-addressed, no VM)")
	}
}

// TestMaterialiseFromSnapshot_RejectsCorruptBlob pins the recreate integrity gate:
// a blob whose on-disk CONTENT no longer matches its content-address filename must
// be refused (snapshot_blob_corrupt) and never cloned into the new VM's boot disk.
// Removing the hashFile-verify in materialiseFromSnapshot MUST fail this test.
func TestMaterialiseFromSnapshot_RejectsCorruptBlob(t *testing.T) {
	m := newTestManager(t)
	poolName := m.defaultTestPool()
	root := m.defaultTestPoolRoot(t)

	// Name the blob with the digest of bodyA, but write bodyB into it (corruption /
	// tamper): content hash != filename digest.
	bodyA := qcow2Body(0x11)
	sumA := sha256.Sum256(bodyA)
	claimed := hex.EncodeToString(sumA[:])
	dir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	blobPath := filepath.Join(dir, claimed+".qcow2")
	if err := os.WriteFile(blobPath, qcow2Body(0x22), 0o600); err != nil { // wrong content
		t.Fatalf("write corrupt blob: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "boot.qcow2")
	v := &VM{ID: uuid.New(), Name: "victim", PoolName: poolName, DiskPath: dst, Architecture: qemu.HostArch()}
	failCode, _ := m.materialiseFromSnapshot(v, &SnapshotRef{
		Pool:  poolName,
		Disks: []SnapshotDiskRef{{Index: 0, Device: "virtio0", SHA256: claimed}},
	})
	if failCode != "snapshot_blob_corrupt" {
		t.Errorf("failCode = %q, want snapshot_blob_corrupt (corrupt blob must not be cloned)", failCode)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Errorf("boot disk was created from a corrupt blob; want no clone")
	}
}

// TestDeleteSnapshotByID_RejectsTraversalName pins the agent-edge path-traversal
// guard: a snapshot name containing a path separator must be rejected before any
// filesystem path is built (defense in depth; the agent does not trust the CP).
func TestDeleteSnapshotByID_RejectsTraversalName(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.DeleteSnapshotByID(context.Background(), uuid.New(), "x/../../etc/passwd", nil); err == nil {
		t.Errorf("DeleteSnapshotByID accepted a traversal snapshot name; want rejection")
	}
}

// TestDeleteSnapshotByID_DropsNonHexDigests pins the agent-edge digest guard: a
// non-content-address (traversal-shaped) orphaned digest must be DROPPED, never fed
// to os.Remove. A decoy file reachable only via the traversal digest must survive.
// Removing the isHexSHA256Lower filter MUST fail this test (the decoy gets deleted).
func TestDeleteSnapshotByID_DropsNonHexDigests(t *testing.T) {
	m := newTestManager(t)
	v := m.seedStoppedVM(t, "delvm")
	root := m.defaultTestPoolRoot(t)

	// A real orphaned blob (valid digest) the agent SHOULD remove.
	good := shaHex([]byte("good-orphan"))
	m.writeManifestOnDisk(t, root, v, "snapX", good)
	// A decoy at {poolRoot}/evil.qcow2, reachable from snapshots/ via "../evil".
	decoy := filepath.Join(root, "evil.qcow2")
	if err := os.WriteFile(decoy, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}

	task, err := m.DeleteSnapshotByID(context.Background(), v.ID, "snapX", []string{good, "../evil"})
	if err != nil {
		t.Fatalf("DeleteSnapshotByID: %v", err)
	}
	_ = m.awaitTask(t, task.ID)

	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("decoy evil.qcow2 was removed via a traversal digest; the non-hex filter must drop it")
	}
	if blobExists(root, good) {
		t.Errorf("valid orphaned blob %s not removed; the GC should still act on hex digests", good)
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

	task, err := m.DeleteSnapshotByID(context.Background(), v.ID, "snapA", []string{orphan})
	if err != nil {
		t.Fatalf("DeleteSnapshotByID: %v", err)
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
