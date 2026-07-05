// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedSnapshot writes a creating snapshot row via the real CreateSnapshot path
// (row + name guard + owner index + per-VM index + backing task), returning the
// row. It is the lowest-friction way to drive ListSnapshots/DeleteSnapshot tests
// off real index entries rather than hand-rolled keys.
func seedSnapshot(t *testing.T, s *etcdstore.Store, ctx context.Context, vmID, ownerID uuid.UUID, name string) store.Snapshot {
	t.Helper()
	sid := uuid.New()
	taskID := uuid.New()
	snap, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: sid, VmID: vmID, OwnerID: ownerID, Name: name,
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &sid,
		},
	}, stubSnapArgs{TaskID: taskID, SnapshotID: sid})
	if err != nil {
		t.Fatalf("seedSnapshot(%q): %v", name, err)
	}
	return snap
}

func TestListSnapshots_CursorDescByVM(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaplist")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-list-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	// Three snapshots with strictly increasing CreatedAt so DESC order is
	// deterministic (patch CreatedAt directly to control ordering).
	var snaps []store.Snapshot
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "snap-"+uuid.NewString()[:8])
		snap.CreatedAt = base.Add(time.Duration(i) * time.Second)
		snap.UpdatedAt = snap.CreatedAt
		if err := cl.PutJSON(ctx, etcd.Key("snapshots", snap.ID.String()), snap); err != nil {
			t.Fatalf("patch CreatedAt: %v", err)
		}
		snaps = append(snaps, snap)
	}
	// Expected DESC by CreatedAt: index 2, 1, 0.
	newest, middle, oldest := snaps[2], snaps[1], snaps[0]

	page1, err := s.ListSnapshots(ctx, store.ListSnapshotsParams{VmID: &vm.ID, LimitCount: 2})
	if err != nil {
		t.Fatalf("ListSnapshots page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if page1[0].ID != newest.ID || page1[1].ID != middle.ID {
		t.Errorf("page1 = [%v %v], want [%v %v] (DESC by created_at)", page1[0].ID, page1[1].ID, newest.ID, middle.ID)
	}

	page2, err := s.ListSnapshots(ctx, store.ListSnapshotsParams{
		VmID: &vm.ID, LimitCount: 2,
		CursorCreatedAt: &page1[1].CreatedAt, CursorID: &page1[1].ID,
	})
	if err != nil {
		t.Fatalf("ListSnapshots page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != oldest.ID {
		t.Errorf("page2 = %v, want [%v] (the 3rd/oldest)", page2, oldest.ID)
	}
}

// TestListSnapshots_OwnerAndVMConjunctive proves the owner pin and the VM filter
// are ANDed, so a developer (ScopeOwn -> OwnerID pinned to caller.ID) cannot widen
// scope by passing another owner's VM id. Two owners A and B each own a VM with one
// snapshot. ListSnapshots{VmID: &vmB, OwnerID: &ownerA} must return ZERO rows (B's
// VM is not A's), and ListSnapshots{VmID: &vmA, OwnerID: &ownerA} returns A's.
func TestListSnapshots_OwnerAndVMConjunctive(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	ownerA, err := s.CreateUser(ctx, userParams(uniqueEmail("snapconj-a")))
	if err != nil {
		t.Fatalf("CreateUser A: %v", err)
	}
	ownerB, err := s.CreateUser(ctx, userParams(uniqueEmail("snapconj-b")))
	if err != nil {
		t.Fatalf("CreateUser B: %v", err)
	}
	vmA := vmRow("snap-conj-a")
	vmA.OwnerID = ownerA.ID
	seedVM(t, cl, vmA)
	vmB := vmRow("snap-conj-b")
	vmB.OwnerID = ownerB.ID
	seedVM(t, cl, vmB)

	snapA := seedSnapshot(t, s, ctx, vmA.ID, ownerA.ID, "snap-a")
	seedSnapshot(t, s, ctx, vmB.ID, ownerB.ID, "snap-b")

	// Owner A pinned + B's VM: the VM index drives iteration but the owner filter
	// must still apply, so A sees ZERO of B's snapshots (no own-scope widening).
	foreign, err := s.ListSnapshots(ctx, store.ListSnapshotsParams{VmID: &vmB.ID, OwnerID: &ownerA.ID, LimitCount: 50})
	if err != nil {
		t.Fatalf("ListSnapshots{vmB, ownerA}: %v", err)
	}
	if len(foreign) != 0 {
		t.Errorf("ListSnapshots{vmB, ownerA} = %d snapshots, want 0 (owner filter must AND the vm filter)", len(foreign))
	}

	// Owner A pinned + A's own VM: returns A's snapshot.
	own, err := s.ListSnapshots(ctx, store.ListSnapshotsParams{VmID: &vmA.ID, OwnerID: &ownerA.ID, LimitCount: 50})
	if err != nil {
		t.Fatalf("ListSnapshots{vmA, ownerA}: %v", err)
	}
	if len(own) != 1 || own[0].ID != snapA.ID {
		t.Errorf("ListSnapshots{vmA, ownerA} = %+v, want only snapshot A %v", own, snapA.ID)
	}
}

func TestSnapshotByID_NotFoundWhenDeleted(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapdel")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-del-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "doomed")
	if _, err := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := s.SnapshotByID(ctx, snap.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SnapshotByID after delete = %v, want store.ErrNotFound", err)
	}
}

func TestUpdateSnapshotMeta_Rename(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaprename")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-rename-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	newName := "weekly"
	updated, err := s.UpdateSnapshotMeta(ctx, store.UpdateSnapshotMetaParams{ID: snap.ID, Name: &newName})
	if err != nil {
		t.Fatalf("UpdateSnapshotMeta: %v", err)
	}
	if updated.Name != "weekly" {
		t.Errorf("renamed name = %q, want weekly", updated.Name)
	}
	if !updated.UpdatedAt.After(snap.UpdatedAt) && !updated.UpdatedAt.Equal(snap.UpdatedAt) {
		t.Errorf("UpdatedAt not bumped: %v -> %v", snap.UpdatedAt, updated.UpdatedAt)
	}

	// The old name "daily" is now reusable within the same VM.
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: uuid.New(), VmID: vm.ID, OwnerID: owner.ID, Name: "daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task:              store.CreateTaskParams{ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending},
	}, stubSnapArgs{}); err != nil {
		t.Errorf("recreate with freed old name: %v", err)
	}
}

func TestUpdateSnapshotMeta_CaseOnlyRename(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapcase")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-case-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	// A case-only rename "daily" -> "Daily" lowercases to the same guard key;
	// it must take the plain-put branch (not a guard-move txn that would issue
	// OpPut+OpDelete on one key and be rejected as a duplicate key).
	newName := "Daily"
	updated, err := s.UpdateSnapshotMeta(ctx, store.UpdateSnapshotMetaParams{ID: snap.ID, Name: &newName})
	if err != nil {
		t.Fatalf("UpdateSnapshotMeta case-only rename: %v", err)
	}
	if updated.Name != "Daily" {
		t.Errorf("renamed name = %q, want Daily (new display case persisted)", updated.Name)
	}
	read, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil || read.Name != "Daily" {
		t.Fatalf("SnapshotByID after case-only rename = (%+v, %v); want name Daily", read, err)
	}
}

func TestDeleteSnapshot_FailsClosedWithChildren(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapchild")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-child-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	parent := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "parent")
	// Nothing currently sets ParentSnapshotID, so craft a child by creating a
	// real snapshot (so its per-VM index entry exists) then patching its
	// ParentSnapshotID to point at the parent.
	child := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "child")
	child.ParentSnapshotID = &parent.ID
	if err := cl.PutJSON(ctx, etcd.Key("snapshots", child.ID.String()), child); err != nil {
		t.Fatalf("patch child ParentSnapshotID: %v", err)
	}

	if _, err := s.DeleteSnapshot(ctx, parent.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); !errors.Is(err, store.ErrSnapshotHasChildren) {
		t.Fatalf("DeleteSnapshot(parent) = %v, want store.ErrSnapshotHasChildren", err)
	}
	// Fail-closed: the parent row is NOT soft-deleted.
	got, err := s.SnapshotByID(ctx, parent.ID)
	if err != nil {
		t.Fatalf("SnapshotByID(parent) after refused delete: %v", err)
	}
	if got.DeletedAt != nil || got.Status != store.SnapshotStatusCreating {
		t.Errorf("parent mutated by refused delete: deletedAt=%v status=%q", got.DeletedAt, got.Status)
	}
}

// TestDeleteSnapshot_FailsClosedWhileVMCreateSourcing proves the delete refuses
// (fail-closed, mutating nothing) while an active vm.create task references the
// snapshot as its source, then succeeds once that create reaches a terminal
// state. A create-from-snapshot pulls the source blobs at the target node while
// the create task runs; deleting the source mid-create could GC a blob the
// in-flight create still needs, so delete must refuse until the create settles.
func TestDeleteSnapshot_FailsClosedWhileVMCreateSourcing(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapsrccreate")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-srccreate-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "base")

	// An active vm.create sourcing this snapshot.
	createTaskID := uuid.New()
	createRes := uuid.New()
	if _, err := s.EnqueueTask(ctx, store.CreateTaskParams{
		ID: createTaskID, Type: "vm.create", Status: store.TaskStatusPending, ResourceType: "vm",
		ResourceID: &createRes, Args: []byte(`{"source_snapshot_id":"` + snap.ID.String() + `"}`), MaxAttempts: 25,
	}, stubVMCreateArgs{SourceSnapshotID: &snap.ID}); err != nil {
		t.Fatalf("EnqueueTask(vm.create): %v", err)
	}

	_, err = s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{})
	if !errors.Is(err, store.ErrSnapshotSourcingCreate) {
		t.Fatalf("DeleteSnapshot while create active = %v, want store.ErrSnapshotSourcingCreate", err)
	}
	// Fail-closed: the row is NOT soft-deleted.
	got, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByID after refused delete: %v", err)
	}
	if got.DeletedAt != nil || got.Status != store.SnapshotStatusCreating {
		t.Errorf("snapshot mutated by refused delete: deletedAt=%v status=%q", got.DeletedAt, got.Status)
	}

	// Settle the create; delete now proceeds.
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: createTaskID, Status: store.TaskStatusSuccess}); err != nil {
		t.Fatalf("UpdateTaskFinalized(create): %v", err)
	}
	if _, err := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); err != nil {
		t.Fatalf("DeleteSnapshot after create settled: %v", err)
	}
	deleted, err := s.SnapshotByIDIncludingDeleted(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByIDIncludingDeleted: %v", err)
	}
	if deleted.DeletedAt == nil || deleted.Status != store.SnapshotStatusDeleting {
		t.Errorf("snapshot not soft-deleted after create settled: deletedAt=%v status=%q", deleted.DeletedAt, deleted.Status)
	}
}

func TestDeleteSnapshot_SoftDeletesAndDropsOwnerIndex(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapsoft")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-soft-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	if _, err := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	cnt, err := s.CountUserResources(ctx, owner.ID)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if cnt.Snapshots != 0 {
		t.Errorf("CountUserResources.Snapshots = %d, want 0 after delete", cnt.Snapshots)
	}

	// The name is reusable within the VM (the guard was dropped).
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: uuid.New(), VmID: vm.ID, OwnerID: owner.ID, Name: "daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task:              store.CreateTaskParams{ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending},
	}, stubSnapArgs{}); err != nil {
		t.Errorf("recreate with freed name after delete: %v", err)
	}
}

func TestSnapshotManifestApplied_FillsDisksReadyAndRefgraph(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapmanifest")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-manifest-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	nodeID := uuid.New()
	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	disks := []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
		{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
	}
	if err := s.SnapshotManifestApplied(ctx, snap.ID, nodeID, disks, store.VmStateAtSnapshotRunning); err != nil {
		t.Fatalf("SnapshotManifestApplied: %v", err)
	}

	got, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.Status != store.SnapshotStatusReady {
		t.Errorf("status = %q, want ready", got.Status)
	}
	if got.VMStateAtSnapshot != store.VmStateAtSnapshotRunning {
		t.Errorf("vm_state_at_snapshot = %q, want running (agent-authoritative report)", got.VMStateAtSnapshot)
	}
	if len(got.Disks) != 2 || got.Disks[0].SHA256 != "aa" || got.Disks[1].SHA256 != "bb" {
		t.Errorf("Disks = %+v, want the two reported disks", got.Disks)
	}

	// The reference-graph entries that fail-closed blob GC later reads must exist.
	for _, d := range disks {
		items, err := cl.Range(ctx, etcd.Key("index", "blob_refs", d.SHA256)+"/")
		if err != nil {
			t.Fatalf("Range blob_refs %q: %v", d.SHA256, err)
		}
		if len(items) != 1 || string(items[0].Value) != snap.ID.String() {
			t.Errorf("blob_refs[%q] = %v, want one entry -> %v", d.SHA256, items, snap.ID)
		}
	}

	// The placement seed that lets blob GC find the holder node WITHOUT a VM lookup
	// must exist: one entry per disk under the producing node.
	for _, d := range disks {
		holders, err := s.BlobPlacements(ctx, d.SHA256)
		if err != nil {
			t.Fatalf("BlobPlacements %q: %v", d.SHA256, err)
		}
		if len(holders) != 1 || holders[0] != nodeID {
			t.Errorf("placement[%q] = %v, want [%v] (the producing node)", d.SHA256, holders, nodeID)
		}
	}
}

// TestBlobPlacements_SeedAndRead proves the placement map round-trip the
// VM-independent blob GC depends on: SnapshotManifestApplied seeds an entry under
// the producing node, BlobPlacements reads the holder back, and an unseeded digest
// reads back empty (not an error). Placement entries are intentionally durable (no
// remove path) - they are the reclamation index the blob-reconcile loop prunes, not the delete path.
func TestBlobPlacements_SeedAndRead(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapplacement")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-placement-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	nodeID := uuid.New()
	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	if err := s.SnapshotManifestApplied(ctx, snap.ID, nodeID, []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied: %v", err)
	}

	holders, err := s.BlobPlacements(ctx, "aa")
	if err != nil {
		t.Fatalf("BlobPlacements: %v", err)
	}
	if len(holders) != 1 || holders[0] != nodeID {
		t.Fatalf("BlobPlacements(aa) = %v, want [%v]", holders, nodeID)
	}

	none, err := s.BlobPlacements(ctx, "no-such-digest")
	if err != nil {
		t.Fatalf("BlobPlacements(unseeded): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("BlobPlacements(unseeded) = %v, want empty", none)
	}
}

// TestDereferenceSnapshotBlobs_OnlyOrphansUnreferenced proves the fail-closed GC
// orphan computation: two snapshots both reference blob "bb" (content-addressed
// dedup); only snap1 references blob "aa". Dereferencing snap1 must orphan ONLY
// "aa" (its sole ref now gone) and NEVER "bb" (snap2 still references it). The
// blobRef entry for ("bb", snap1) is removed; ("bb", snap2) survives.
func TestDereferenceSnapshotBlobs_OnlyOrphansUnreferenced(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapderef")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-deref-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap1 := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "s1")
	snap2 := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "s2")

	// snap1: blobs aa (sole) + bb (shared). snap2: blob bb (shared).
	nodeID := uuid.New()
	if err := s.SnapshotManifestApplied(ctx, snap1.ID, nodeID, []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
		{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied snap1: %v", err)
	}
	if err := s.SnapshotManifestApplied(ctx, snap2.ID, nodeID, []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied snap2: %v", err)
	}

	orphaned, err := s.DereferenceSnapshotBlobs(ctx, snap1.ID, []string{"aa", "bb"})
	if err != nil {
		t.Fatalf("DereferenceSnapshotBlobs: %v", err)
	}
	if len(orphaned) != 1 || orphaned[0] != "aa" {
		t.Errorf("orphaned = %v, want only [aa] (bb still referenced by snap2)", orphaned)
	}

	// aa has no refs left; bb retains snap2's ref (snap1's ref was removed).
	aaRefs, err := cl.Range(ctx, etcd.Key("index", "blob_refs", "aa")+"/")
	if err != nil {
		t.Fatalf("Range blob_refs aa: %v", err)
	}
	if len(aaRefs) != 0 {
		t.Errorf("blob_refs[aa] = %v, want empty after dereference", aaRefs)
	}
	bbRefs, err := cl.Range(ctx, etcd.Key("index", "blob_refs", "bb")+"/")
	if err != nil {
		t.Fatalf("Range blob_refs bb: %v", err)
	}
	if len(bbRefs) != 1 || string(bbRefs[0].Value) != snap2.ID.String() {
		t.Errorf("blob_refs[bb] = %v, want exactly snap2's ref %v", bbRefs, snap2.ID)
	}
}

// stubSnapArgs is a minimal queue.JobArgs the snapshot enqueue path can marshal
// onto the backing job. The concrete SnapshotCreateArgs lives in the handlers
// package (built in a later task); CreateSnapshot takes the queue.JobArgs
// interface, so any Kind()-implementer works here.
type stubSnapArgs struct {
	TaskID     uuid.UUID
	SnapshotID uuid.UUID
}

func (stubSnapArgs) Kind() string { return "vm.snapshot.create" }

// stubVMCreateArgs is a minimal queue.JobArgs the vm.create enqueue path can
// marshal: it carries only source_snapshot_id, the field
// ActiveCreatesReferencingSnapshot reads off the task Args.
type stubVMCreateArgs struct {
	SourceSnapshotID *uuid.UUID `json:"source_snapshot_id,omitempty"`
}

func (stubVMCreateArgs) Kind() string { return "vm.create" }

func TestCreateSnapshot_WritesRowOwnerIndexAndTask(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapowner")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	sid := uuid.New()
	taskID := uuid.New()
	got, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: sid, VmID: vm.ID, OwnerID: owner.ID, Name: "daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &sid, MaxAttempts: 25, CreatedBy: &owner.ID,
		},
	}, stubSnapArgs{TaskID: taskID, SnapshotID: sid})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if got.ID != sid || got.Status != store.SnapshotStatusCreating {
		t.Errorf("CreateSnapshot = {id:%v status:%q}, want {id:%v status:creating}", got.ID, got.Status, sid)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("CreateSnapshot timestamps not stamped: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
	if len(got.Disks) != 0 {
		t.Errorf("CreateSnapshot Disks = %v, want empty (filled by worker on success)", got.Disks)
	}

	// Primary row reads back at status=creating.
	read, err := s.SnapshotByID(ctx, sid)
	if err != nil || read.Status != store.SnapshotStatusCreating {
		t.Fatalf("SnapshotByID = (%+v, %v); want status creating", read, err)
	}

	// The owner-index entry drives CountUserResources.
	cnt, err := s.CountUserResources(ctx, owner.ID)
	if err != nil {
		t.Fatalf("CountUserResources: %v", err)
	}
	if cnt.Snapshots != 1 {
		t.Errorf("CountUserResources.Snapshots = %d, want 1", cnt.Snapshots)
	}

	// The backing task was enqueued atomically and is readable.
	tsk, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID(backing task): %v", err)
	}
	if tsk.Type != "vm.snapshot.create" || tsk.Status != store.TaskStatusPending {
		t.Errorf("backing task = {type:%q status:%q}, want {vm.snapshot.create pending}", tsk.Type, tsk.Status)
	}

	// A duplicate name within the same VM (different case) is rejected by the guard.
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: uuid.New(), VmID: vm.ID, OwnerID: owner.ID, Name: "Daily",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
		},
	}, stubSnapArgs{}); !errors.Is(err, store.ErrSnapshotNameExists) {
		t.Errorf("duplicate name err = %v, want store.ErrSnapshotNameExists", err)
	}
}

// TestMarkSnapshotError_CreatingToError flips a creating row to error with the
// message and bumps updated_at, leaving every other field intact.
func TestMarkSnapshotError_CreatingToError(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaperr")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-err-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "doomed")
	if snap.Status != store.SnapshotStatusCreating {
		t.Fatalf("seeded status = %q, want creating", snap.Status)
	}

	if err := s.MarkSnapshotError(ctx, snap.ID, "agent capture failed"); err != nil {
		t.Fatalf("MarkSnapshotError: %v", err)
	}

	got, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.Status != store.SnapshotStatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "agent capture failed" {
		t.Errorf("error_message = %v, want %q", got.ErrorMessage, "agent capture failed")
	}
	if !got.UpdatedAt.After(snap.UpdatedAt) && !got.UpdatedAt.Equal(snap.UpdatedAt) {
		t.Errorf("updated_at = %v, want >= seeded %v", got.UpdatedAt, snap.UpdatedAt)
	}
}

// TestMarkSnapshotError_AbsentRowNoOp returns nil for an absent snapshot id and
// writes nothing (no resurrection of a never-existed row).
func TestMarkSnapshotError_AbsentRowNoOp(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	missing := uuid.New()
	if err := s.MarkSnapshotError(ctx, missing, "boom"); err != nil {
		t.Fatalf("MarkSnapshotError(absent) = %v, want nil", err)
	}
	if _, err := s.SnapshotByIDIncludingDeleted(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("absent row materialised: SnapshotByIDIncludingDeleted = %v, want ErrNotFound", err)
	}
}

// TestMarkSnapshotError_SoftDeletedNoOp must NOT resurrect or mutate a
// soft-deleted row (delete leaves the row soft-deleted at status=deleting).
func TestMarkSnapshotError_SoftDeletedNoOp(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaperrdel")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-err-del-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "doomed")
	if _, err := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	if err := s.MarkSnapshotError(ctx, snap.ID, "boom"); err != nil {
		t.Fatalf("MarkSnapshotError(soft-deleted) = %v, want nil", err)
	}

	got, err := s.SnapshotByIDIncludingDeleted(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByIDIncludingDeleted: %v", err)
	}
	if got.DeletedAt == nil {
		t.Errorf("soft-deleted row resurrected: deleted_at cleared")
	}
	if got.Status != store.SnapshotStatusDeleting {
		t.Errorf("status = %q, want deleting (unchanged); MarkSnapshotError must not clobber a soft-deleted row", got.Status)
	}
	if got.ErrorMessage != nil {
		t.Errorf("error_message = %v, want nil (must not write on a soft-deleted row)", got.ErrorMessage)
	}
}

// TestMarkSnapshotError_ReadyNoClobber must NOT overwrite a row that already
// reached ready (a redelivered failure must never clobber a committed success).
func TestMarkSnapshotError_ReadyNoClobber(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaperrready")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-err-ready-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "good")
	snap.Status = store.SnapshotStatusReady
	if err := cl.PutJSON(ctx, etcd.Key("snapshots", snap.ID.String()), snap); err != nil {
		t.Fatalf("patch ready: %v", err)
	}

	if err := s.MarkSnapshotError(ctx, snap.ID, "boom"); err != nil {
		t.Fatalf("MarkSnapshotError(ready) = %v, want nil", err)
	}

	got, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.Status != store.SnapshotStatusReady {
		t.Errorf("status = %q, want ready (unchanged); MarkSnapshotError must not clobber a committed row", got.Status)
	}
	if got.ErrorMessage != nil {
		t.Errorf("error_message = %v, want nil (must not write on a ready row)", got.ErrorMessage)
	}
}

// TestMarkSnapshotError_CapsMessage bounds an oversized agent error string so a
// huge message cannot bloat the row.
func TestMarkSnapshotError_CapsMessage(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaperrcap")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-err-cap-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "doomed")
	huge := strings.Repeat("x", 10_000)
	if err := s.MarkSnapshotError(ctx, snap.ID, huge); err != nil {
		t.Fatalf("MarkSnapshotError: %v", err)
	}

	got, err := s.SnapshotByID(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.ErrorMessage == nil {
		t.Fatalf("error_message = nil, want a (capped) message")
	}
	if len(*got.ErrorMessage) >= len(huge) {
		t.Errorf("error_message len = %d, want bounded below %d", len(*got.ErrorMessage), len(huge))
	}
}

// TestSnapshotManifestApplied_SoftDeletedNoResurrect pins the abort half of the
// CAS fix: once a DeleteSnapshot has soft-deleted the row, a (redelivered)
// SnapshotManifestApplied must NOT resurrect it to ready. The old code read via
// SnapshotByID and blind-put the pre-read value back; the fix re-reads under CAS,
// sees DeletedAt, and returns a no-op.
func TestSnapshotManifestApplied_SoftDeletedNoResurrect(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapresurrect")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-resurrect-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "doomed")
	if _, err := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	disks := []store.SnapshotDisk{{Index: 0, Device: "virtio0", SHA256: "cc", SizeBytes: 10, Format: "qcow2"}}
	if err := s.SnapshotManifestApplied(ctx, snap.ID, uuid.New(), disks, store.VmStateAtSnapshotRunning); err != nil {
		t.Fatalf("SnapshotManifestApplied on a soft-deleted row = %v, want nil (abort, no resurrect)", err)
	}

	// The row stays soft-deleted (not resurrected to ready).
	if _, err := s.SnapshotByID(ctx, snap.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SnapshotByID after manifest-on-deleted = %v, want ErrNotFound (row must stay deleted)", err)
	}
	deleted, err := s.SnapshotByIDIncludingDeleted(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByIDIncludingDeleted: %v", err)
	}
	if deleted.DeletedAt == nil || deleted.Status == store.SnapshotStatusReady {
		t.Errorf("row = {status:%q deleted_at:%v}, want soft-deleted (never resurrected to ready)", deleted.Status, deleted.DeletedAt)
	}
	// The refgraph must NOT have been seeded for a deleted snapshot.
	items, err := cl.Range(ctx, etcd.Key("index", "blob_refs", "cc")+"/")
	if err != nil {
		t.Fatalf("Range blob_refs: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("blob_refs[cc] = %v, want empty (aborted projection seeds no refgraph)", items)
	}
}

// TestSnapshotManifestAppliedVsDelete_NoTornState runs the projection and the
// delete concurrently many times and asserts the durable state never tears. The
// invariant: a row that is still LIVE (SnapshotByID returns it) must still be
// counted through the owner index. A resurrection (the projection blind-putting
// ready AFTER the delete dropped the owner index) would show a live row with a
// zero owner-resource count - the exact torn state the CAS prevents.
func TestSnapshotManifestAppliedVsDelete_NoTornState(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snaptorn")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-torn-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	disks := []store.SnapshotDisk{{Index: 0, Device: "virtio0", SHA256: "dd", SizeBytes: 10, Format: "qcow2"}}
	for i := 0; i < 60; i++ {
		snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "race-"+uuid.NewString()[:8])

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.SnapshotManifestApplied(ctx, snap.ID, uuid.New(), disks, store.VmStateAtSnapshotRunning)
		}()
		go func() {
			defer wg.Done()
			_, _ = s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{})
		}()
		wg.Wait()

		_, liveErr := s.SnapshotByID(ctx, snap.ID)
		cnt, err := s.CountUserResources(ctx, owner.ID)
		if err != nil {
			t.Fatalf("CountUserResources: %v", err)
		}
		// If the row is live (not soft-deleted) it must be counted; a resurrected row
		// whose owner index the delete dropped is a torn state.
		if liveErr == nil && cnt.Snapshots == 0 {
			t.Fatalf("iter %d: snapshot %s is live but owner-resource count is 0 (torn/resurrected row)", i, snap.ID)
		}
		// Clean up so CountUserResources reflects only the current iteration's row.
		if liveErr == nil {
			if _, derr := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); derr != nil && !errors.Is(derr, store.ErrNotFound) {
				t.Fatalf("cleanup delete: %v", derr)
			}
		}
	}
}

// TestUpdateSnapshotMeta_SoftDeletedNoResurrect pins the third writer closed by
// the CAS class fix: a metadata patch must NOT resurrect a snapshot a concurrent
// delete already soft-deleted. Sequentially: delete, then patch -> ErrNotFound,
// and the row stays soft-deleted (never blind-put back to a live ready row with
// its owner index already dropped).
func TestUpdateSnapshotMeta_SoftDeletedNoResurrect(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	owner, err := s.CreateUser(ctx, userParams(uniqueEmail("snapmetadel")))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	vm := vmRow("snap-metadel-src")
	vm.OwnerID = owner.ID
	seedVM(t, cl, vm)

	snap := seedSnapshot(t, s, ctx, vm.ID, owner.ID, "daily")
	if _, err := s.DeleteSnapshot(ctx, snap.ID, taskParams(store.TaskStatusPending, nil), stubSnapArgs{}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	// A rename patch (guard-move branch) on the deleted row must not resurrect it.
	newName := "weekly"
	if _, err := s.UpdateSnapshotMeta(ctx, store.UpdateSnapshotMetaParams{ID: snap.ID, Name: &newName}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateSnapshotMeta(rename) on a soft-deleted row = %v, want ErrNotFound (no resurrect)", err)
	}
	// A description patch (plain-put branch) likewise.
	desc := "note"
	if _, err := s.UpdateSnapshotMeta(ctx, store.UpdateSnapshotMetaParams{ID: snap.ID, Description: &desc}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateSnapshotMeta(description) on a soft-deleted row = %v, want ErrNotFound (no resurrect)", err)
	}

	if _, err := s.SnapshotByID(ctx, snap.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SnapshotByID after patch-on-deleted = %v, want ErrNotFound (row stays deleted)", err)
	}
	deleted, err := s.SnapshotByIDIncludingDeleted(ctx, snap.ID)
	if err != nil {
		t.Fatalf("SnapshotByIDIncludingDeleted: %v", err)
	}
	if deleted.DeletedAt == nil {
		t.Errorf("row resurrected: DeletedAt = nil, want still soft-deleted")
	}
}
