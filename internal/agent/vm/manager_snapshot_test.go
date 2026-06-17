// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
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

func (f *fakeCapturer) Capture(_ context.Context, v *VM, device, dest string) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	src := v.DiskPath
	if f.srcOverride != nil {
		if o, ok := f.srcOverride[device]; ok {
			src = o
		}
	}
	f.mu.Unlock()
	b, err := os.ReadFile(src) //nolint:gosec // test-local path
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
