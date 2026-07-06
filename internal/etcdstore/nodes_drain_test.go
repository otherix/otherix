// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// fakeDrainArgs is a minimal node.drain job payload for exercising the drain
// state-flip primitives; only Kind matters to the enqueue path.
type fakeDrainArgs struct {
	TaskID       uuid.UUID
	NodeID       uuid.UUID
	DeadlineUnix int64
}

func (fakeDrainArgs) Kind() string { return "node.drain" }

// seedNodeWithStatus creates a node row in the given status via CreateNode.
func seedNodeWithStatus(t *testing.T, s nodeCreator, ctx context.Context, name string, status store.NodeStatus) store.Node {
	t.Helper()
	p := nodeParams(uniqueNodeName(name))
	p.Status = status
	n, err := s.CreateNode(ctx, p)
	if err != nil {
		t.Fatalf("CreateNode(%s): %v", status, err)
	}
	return n
}

// nodeCreator is the narrow store surface seedNodeWithStatus needs.
type nodeCreator interface {
	CreateNode(ctx context.Context, arg store.CreateNodeParams) (store.Node, error)
}

func TestStartNodeDrainFlipsReadyToDraining(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-ready", store.NodeStatusReady)

	taskID := uuid.New()
	task, err := s.StartNodeDrain(ctx, node.ID, store.CreateTaskParams{
		ID:           taskID,
		Type:         "node.drain",
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &node.ID,
		MaxAttempts:  1,
	}, fakeDrainArgs{TaskID: taskID, NodeID: node.ID, DeadlineUnix: 1 << 40})
	if err != nil {
		t.Fatalf("StartNodeDrain: %v", err)
	}
	if task.ID != taskID {
		t.Errorf("StartNodeDrain task.ID = %v, want %v", task.ID, taskID)
	}

	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusDraining {
		t.Errorf("node status = %v, want draining", got.Status)
	}
	if got.DrainTaskID == nil || *got.DrainTaskID != taskID {
		t.Errorf("node DrainTaskID = %v, want %v", got.DrainTaskID, taskID)
	}
}

func TestStartNodeDrainRejectsPending(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-pending", store.NodeStatusPending)

	taskID := uuid.New()
	_, err := s.StartNodeDrain(ctx, node.ID, store.CreateTaskParams{
		ID:           taskID,
		Type:         "node.drain",
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &node.ID,
		MaxAttempts:  1,
	}, fakeDrainArgs{TaskID: taskID, NodeID: node.ID, DeadlineUnix: 1 << 40})
	if !errors.Is(err, store.ErrNodeNotDrainable) {
		t.Errorf("StartNodeDrain(pending) = %v, want store.ErrNodeNotDrainable", err)
	}
}

func TestFinishNodeDrainFlipsDrainingToCordoned(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-finish", store.NodeStatusReady)

	taskID := uuid.New()
	if _, err := s.StartNodeDrain(ctx, node.ID, store.CreateTaskParams{
		ID:           taskID,
		Type:         "node.drain",
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &node.ID,
		MaxAttempts:  1,
	}, fakeDrainArgs{TaskID: taskID, NodeID: node.ID, DeadlineUnix: 1 << 40}); err != nil {
		t.Fatalf("StartNodeDrain: %v", err)
	}

	// First finalize records a DISTINCT terminal status + result, flips the
	// draining node to cordoned, and clears the pointer.
	if err := s.FinishNodeDrain(ctx, node.ID, taskID, store.TaskStatusFailed, []byte(`{"remaining":3}`)); err != nil {
		t.Fatalf("FinishNodeDrain: %v", err)
	}

	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusCordoned {
		t.Errorf("node status = %v, want cordoned", got.Status)
	}
	if got.DrainTaskID != nil {
		t.Errorf("node DrainTaskID = %v, want nil", got.DrainTaskID)
	}

	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusFailed {
		t.Errorf("task status = %v, want failed", task.Status)
	}
	if string(task.Result) != `{"remaining":3}` {
		t.Errorf("task result = %s, want {\"remaining\":3}", task.Result)
	}

	// A second finalize with a DIFFERENT terminal status + result is an
	// idempotent no-op: the task already reached a terminal status, so the
	// stored row must still read the FIRST status + result. This bites the
	// isTerminalTaskStatus short-circuit - without it the second call would
	// overwrite failed -> success.
	if err := s.FinishNodeDrain(ctx, node.ID, taskID, store.TaskStatusSuccess, []byte(`{"remaining":0}`)); err != nil {
		t.Errorf("second FinishNodeDrain = %v, want nil", err)
	}
	task, err = s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID after redelivery: %v", err)
	}
	if task.Status != store.TaskStatusFailed {
		t.Errorf("task status after redelivery = %v, want failed (unchanged)", task.Status)
	}
	if string(task.Result) != `{"remaining":3}` {
		t.Errorf("task result after redelivery = %s, want {\"remaining\":3} (unchanged)", task.Result)
	}
}

// TestFinishNodeDrainClearsDanglingPointerWithoutFlippingStatus exercises the
// branch where the node is no longer draining (it went unreachable out from
// under the saga) but still carries a drain_task_id. FinishNodeDrain must clear
// the dangling pointer and finalize the task WITHOUT forcing the node back to
// cordoned. The node is moved to unreachable by a direct row write (the same
// raw PutJSON the heartbeat/maintenance paths use) that leaves DrainTaskID set,
// reproducing the real "node went unreachable mid-drain" condition.
func TestFinishNodeDrainClearsDanglingPointerWithoutFlippingStatus(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-dangling", store.NodeStatusReady)

	taskID := uuid.New()
	if _, err := s.StartNodeDrain(ctx, node.ID, store.CreateTaskParams{
		ID:           taskID,
		Type:         "node.drain",
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &node.ID,
		MaxAttempts:  1,
	}, fakeDrainArgs{TaskID: taskID, NodeID: node.ID, DeadlineUnix: 1 << 40}); err != nil {
		t.Fatalf("StartNodeDrain: %v", err)
	}

	// Force the node out of draining into unreachable while preserving the
	// drain_task_id, reproducing a node that went unreachable mid-drain.
	var row store.Node
	if _, err := cli.GetJSON(ctx, etcd.Key("nodes", node.ID.String()), &row); err != nil {
		t.Fatalf("GetJSON node: %v", err)
	}
	if row.DrainTaskID == nil || *row.DrainTaskID != taskID {
		t.Fatalf("precondition: node DrainTaskID = %v, want %v", row.DrainTaskID, taskID)
	}
	row.Status = store.NodeStatusUnreachable
	if err := cli.PutJSON(ctx, etcd.Key("nodes", node.ID.String()), row); err != nil {
		t.Fatalf("PutJSON node: %v", err)
	}

	if err := s.FinishNodeDrain(ctx, node.ID, taskID, store.TaskStatusSuccess, []byte(`{"remaining":0}`)); err != nil {
		t.Fatalf("FinishNodeDrain: %v", err)
	}

	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusUnreachable {
		t.Errorf("node status = %v, want unreachable (NOT forced to cordoned)", got.Status)
	}
	if got.DrainTaskID != nil {
		t.Errorf("node DrainTaskID = %v, want nil (dangling pointer cleared)", got.DrainTaskID)
	}

	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task status = %v, want success", task.Status)
	}
}

// TestDrainCancelMarker locks the cooperative cancel marker round-trip:
// DrainCancelRequested is false for a fresh task id, and true after
// RequestDrainCancel sets the marker.
func TestDrainCancelMarker(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	taskID := uuid.New()
	requested, err := s.DrainCancelRequested(ctx, taskID)
	if err != nil {
		t.Fatalf("DrainCancelRequested(fresh): %v", err)
	}
	if requested {
		t.Errorf("DrainCancelRequested(fresh) = true, want false")
	}

	if err := s.RequestDrainCancel(ctx, taskID); err != nil {
		t.Fatalf("RequestDrainCancel: %v", err)
	}

	requested, err = s.DrainCancelRequested(ctx, taskID)
	if err != nil {
		t.Fatalf("DrainCancelRequested(after request): %v", err)
	}
	if !requested {
		t.Errorf("DrainCancelRequested(after request) = false, want true")
	}
}

// TestDrainCancelMarkerRoundTripAndFinishDeletes verifies the cooperative
// cancel marker round-trips and that FinishNodeDrain deletes it on finalize.
func TestDrainCancelMarkerRoundTripAndFinishDeletes(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-cancel", store.NodeStatusReady)

	taskID := uuid.New()
	if _, err := s.StartNodeDrain(ctx, node.ID, store.CreateTaskParams{
		ID:           taskID,
		Type:         "node.drain",
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &node.ID,
		MaxAttempts:  1,
	}, fakeDrainArgs{TaskID: taskID, NodeID: node.ID, DeadlineUnix: 1 << 40}); err != nil {
		t.Fatalf("StartNodeDrain: %v", err)
	}

	if err := s.RequestDrainCancel(ctx, taskID); err != nil {
		t.Fatalf("RequestDrainCancel: %v", err)
	}
	requested, err := s.DrainCancelRequested(ctx, taskID)
	if err != nil {
		t.Fatalf("DrainCancelRequested: %v", err)
	}
	if !requested {
		t.Errorf("DrainCancelRequested = false, want true after RequestDrainCancel")
	}

	if err := s.FinishNodeDrain(ctx, node.ID, taskID, store.TaskStatusSuccess, []byte(`{"remaining":0}`)); err != nil {
		t.Fatalf("FinishNodeDrain: %v", err)
	}

	requested, err = s.DrainCancelRequested(ctx, taskID)
	if err != nil {
		t.Fatalf("DrainCancelRequested after finish: %v", err)
	}
	if requested {
		t.Errorf("DrainCancelRequested = true, want false after FinishNodeDrain deletes the marker")
	}
}

// TestDrainCancelMarkerDeletedOnTaskOnlyFinalize covers the finalize path that
// bypasses FinishNodeDrain (a node force-deleted mid-drain finalizes the task
// only). DeleteDrainCancel must remove the cancel marker there too so it does
// not leak. It is also idempotent - a second delete on a fresh id is a no-op.
func TestDrainCancelMarkerDeletedOnTaskOnlyFinalize(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	taskID := uuid.New()

	if err := s.RequestDrainCancel(ctx, taskID); err != nil {
		t.Fatalf("RequestDrainCancel: %v", err)
	}
	if err := s.DeleteDrainCancel(ctx, taskID); err != nil {
		t.Fatalf("DeleteDrainCancel: %v", err)
	}
	requested, err := s.DrainCancelRequested(ctx, taskID)
	if err != nil {
		t.Fatalf("DrainCancelRequested after delete: %v", err)
	}
	if requested {
		t.Errorf("DrainCancelRequested = true, want false after DeleteDrainCancel")
	}
	// Idempotent: deleting an absent marker is a clean no-op.
	if err := s.DeleteDrainCancel(ctx, uuid.New()); err != nil {
		t.Errorf("DeleteDrainCancel(absent) = %v, want nil", err)
	}
}

// TestActiveSourceMigrationCount asserts the count is filtered to migrations
// whose SOURCE is the node: the in-flight evacuations the drain saga started.
// The dst==0 assertion proves the source-only filter - a naive "touching"
// count would return 1 for the target node and fail.
func TestActiveSourceMigrationCount(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	src := seedNodeWithStatus(t, s, ctx, "drain-src-mig", store.NodeStatusReady)
	dst := seedNodeWithStatus(t, s, ctx, "drain-dst-mig", store.NodeStatusReady)
	vm := seedPinnedVM(t, cli, src.ID)

	if n, err := s.ActiveSourceMigrationCount(ctx, src.ID); err != nil || n != 0 {
		t.Fatalf("ActiveSourceMigrationCount(src) before migration = (%d, %v), want (0, nil)", n, err)
	}

	taskID := uuid.New()
	p := store.CreateMigrationParams{
		ID:           uuid.New(),
		VmID:         vm.ID,
		SourceNodeID: &src.ID,
		TargetNodeID: &dst.ID,
		Reason:       store.MigrationReasonDrain,
		Live:         true,
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.migrate",
			Status:       store.TaskStatusPending,
			ResourceType: "migration",
			MaxAttempts:  25,
		},
	}
	if _, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: taskID, MigrationID: p.ID}); err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	if n, err := s.ActiveSourceMigrationCount(ctx, src.ID); err != nil || n != 1 {
		t.Errorf("ActiveSourceMigrationCount(src) = (%d, %v), want (1, nil)", n, err)
	}
	// The target node sees zero SOURCE migrations even though the migration
	// touches it - this is the load-bearing source-only filter assertion.
	if n, err := s.ActiveSourceMigrationCount(ctx, dst.ID); err != nil || n != 0 {
		t.Errorf("ActiveSourceMigrationCount(dst) = (%d, %v), want (0, nil) (counted by source only)", n, err)
	}
}

// refIDs collects the ids returned by ListVMRefsForNodeDeclared into a set so
// tests can assert membership without depending on result order.
func refIDs(refs []store.NodeVMRef) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(refs))
	for _, r := range refs {
		out[r.ID] = true
	}
	return out
}

func TestListVMRefsForNodeDeclaredReturnsPinnedVM(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-refs-pinned", store.NodeStatusReady)

	vm := seedPinnedVM(t, cli, node.ID)

	refs, err := s.ListVMRefsForNodeDeclared(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListVMRefsForNodeDeclared: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("ListVMRefsForNodeDeclared returned %d refs, want 1", len(refs))
	}
	if refs[0].ID != vm.ID || refs[0].Name != vm.Name {
		t.Errorf("ref = {ID:%v, Name:%q}, want {ID:%v, Name:%q}", refs[0].ID, refs[0].Name, vm.ID, vm.Name)
	}
}

// TestListVMRefsForNodeDeclaredIncludesUnobservedVM is the load-bearing case: a
// VM pinned to the node that has NO vm_runtime row (created/pinned but never
// observed by the agent) MUST still be returned. seedPinnedVM writes the vms
// primary plus the pinned_node index entry and never touches the vm_runtime
// node index, so this fails if the method ranges the runtime index.
func TestListVMRefsForNodeDeclaredIncludesUnobservedVM(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-refs-unobserved", store.NodeStatusReady)

	vm := seedPinnedVM(t, cli, node.ID)

	// Guard the construction: no vm_runtime primary and no runtime-by-node index
	// entry exist for this VM, so the only way to find it is the pinned index.
	var rt store.VMRuntime
	if found, err := cli.GetJSON(ctx, etcd.Key("vm_runtime", vm.ID.String()), &rt); err != nil || found {
		t.Fatalf("vm_runtime primary = (found=%v, err=%v), want absent", found, err)
	}
	idx, err := cli.Range(ctx, etcd.Key("index", "vm_runtime", "node", node.ID.String())+"/")
	if err != nil {
		t.Fatalf("range vm_runtime node index: %v", err)
	}
	if len(idx) != 0 {
		t.Fatalf("vm_runtime node index has %d entries, want 0 (VM is unobserved)", len(idx))
	}

	refs, err := s.ListVMRefsForNodeDeclared(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListVMRefsForNodeDeclared: %v", err)
	}
	if !refIDs(refs)[vm.ID] {
		t.Errorf("ListVMRefsForNodeDeclared = %+v, want to include unobserved pinned VM %v", refs, vm.ID)
	}
}

func TestListVMRefsForNodeDeclaredExcludesDeletedVM(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-refs-deleted", store.NodeStatusReady)

	// A live VM that must be returned.
	live := seedPinnedVM(t, cli, node.ID)

	// A desired-deleted VM (user asked for deletion), still pinned: excluded.
	desiredDeleted := seedPinnedVM(t, cli, node.ID)
	desiredDeleted.DesiredPhase = store.VmDesiredPhaseDeleted
	seedVM(t, cli, desiredDeleted)

	// A soft-deleted VM (DeletedAt set), still pinned: excluded.
	softDeleted := seedPinnedVM(t, cli, node.ID)
	now := time.Now().UTC()
	softDeleted.DeletedAt = &now
	seedVM(t, cli, softDeleted)

	// A VM pinned to a DIFFERENT node: excluded.
	other := seedNodeWithStatus(t, s, ctx, "drain-refs-other", store.NodeStatusReady)
	otherVM := seedPinnedVM(t, cli, other.ID)

	refs, err := s.ListVMRefsForNodeDeclared(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListVMRefsForNodeDeclared: %v", err)
	}
	ids := refIDs(refs)
	if !ids[live.ID] {
		t.Errorf("ListVMRefsForNodeDeclared = %+v, want to include live VM %v", refs, live.ID)
	}
	if ids[desiredDeleted.ID] {
		t.Errorf("ListVMRefsForNodeDeclared included desired-deleted VM %v, want excluded", desiredDeleted.ID)
	}
	if ids[softDeleted.ID] {
		t.Errorf("ListVMRefsForNodeDeclared included soft-deleted VM %v, want excluded", softDeleted.ID)
	}
	if ids[otherVM.ID] {
		t.Errorf("ListVMRefsForNodeDeclared included VM %v pinned to another node, want excluded", otherVM.ID)
	}
}

// startDrain seeds a ready node, begins a drain, and returns the node + task id.
func startDrain(t *testing.T, s *etcdstore.Store, ctx context.Context, name string) (store.Node, uuid.UUID) {
	t.Helper()
	node := seedNodeWithStatus(t, s, ctx, name, store.NodeStatusReady)
	taskID := uuid.New()
	if _, err := s.StartNodeDrain(ctx, node.ID, store.CreateTaskParams{
		ID:           taskID,
		Type:         "node.drain",
		Status:       store.TaskStatusPending,
		ResourceType: "node",
		ResourceID:   &node.ID,
		MaxAttempts:  1,
	}, fakeDrainArgs{TaskID: taskID, NodeID: node.ID, DeadlineUnix: 1 << 40}); err != nil {
		t.Fatalf("StartNodeDrain: %v", err)
	}
	return node, taskID
}

// TestCountDrainingNodes asserts the drain-admission counter: zero before any
// drain, two after starting drains on two nodes, and unaffected by ready /
// cordoned nodes that are not draining.
func TestCountDrainingNodes(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if n, err := s.CountDrainingNodes(ctx); err != nil || n != 0 {
		t.Fatalf("CountDrainingNodes() before any drain = (%d, %v), want (0, nil)", n, err)
	}

	// Two nodes that are NOT draining: a ready one and a cordoned one. Neither
	// must be counted.
	seedNodeWithStatus(t, s, ctx, "count-ready", store.NodeStatusReady)
	seedNodeWithStatus(t, s, ctx, "count-cordoned", store.NodeStatusCordoned)

	startDrain(t, s, ctx, "count-draining-1")
	startDrain(t, s, ctx, "count-draining-2")

	if n, err := s.CountDrainingNodes(ctx); err != nil || n != 2 {
		t.Errorf("CountDrainingNodes() = (%d, %v), want (2, nil)", n, err)
	}
}

// TestReconcileStuckDrainCordonsTerminalTask drains a node, forces its drain
// task terminal-failed while the node stays draining (the wedge), and asserts
// the backstop finalizes the node to cordoned and clears the dangling pointer.
func TestReconcileStuckDrainCordonsTerminalTask(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node, taskID := startDrain(t, s, ctx, "drain-wedged-terminal")

	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     taskID,
		Status: store.TaskStatusFailed,
	}); err != nil {
		t.Fatalf("force task failed: %v", err)
	}

	n, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult)
	if err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}
	if n != 1 {
		t.Errorf("ReconcileStuckDrain() = %d, want 1", n)
	}
	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusCordoned {
		t.Errorf("node status = %v, want cordoned", got.Status)
	}
	if got.DrainTaskID != nil {
		t.Errorf("node DrainTaskID = %v, want nil", got.DrainTaskID)
	}
}

// TestReconcileStuckDrainCordonsMissingTask drains a node, then deletes the
// drain task row (reproducing retention reaping the task while the node stayed
// draining). With its drain_task_id pointing at a now-missing task, TaskByID
// returns store.ErrNotFound and the backstop cordons the node.
func TestReconcileStuckDrainCordonsMissingTask(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node, taskID := startDrain(t, s, ctx, "drain-wedged-missing")

	deleted, err := cli.Delete(ctx, etcd.Key("tasks", taskID.String()))
	if err != nil {
		t.Fatalf("delete task row: %v", err)
	}
	if !deleted {
		t.Fatalf("delete task row: task %v not present", taskID)
	}
	if _, terr := s.TaskByID(ctx, taskID); !errors.Is(terr, store.ErrNotFound) {
		t.Fatalf("precondition: TaskByID(deleted) = %v, want store.ErrNotFound", terr)
	}

	n, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult)
	if err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}
	if n != 1 {
		t.Errorf("ReconcileStuckDrain() = %d, want 1", n)
	}
	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusCordoned || got.DrainTaskID != nil {
		t.Errorf("node = {%v, drain_task_id=%v}, want cordoned/nil", got.Status, got.DrainTaskID)
	}
}

// TestReconcileStuckDrainCordonsTornNilPointer writes a draining node row with a
// nil drain_task_id directly (the torn state: draining with nothing driving it)
// and asserts the backstop cordons it. The row is written via the same raw
// PutJSON the heartbeat/maintenance paths use.
func TestReconcileStuckDrainCordonsTornNilPointer(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "drain-torn-nil", store.NodeStatusReady)

	var row store.Node
	if _, err := cli.GetJSON(ctx, etcd.Key("nodes", node.ID.String()), &row); err != nil {
		t.Fatalf("GetJSON node: %v", err)
	}
	row.Status = store.NodeStatusDraining
	row.DrainTaskID = nil
	if err := cli.PutJSON(ctx, etcd.Key("nodes", node.ID.String()), row); err != nil {
		t.Fatalf("PutJSON node: %v", err)
	}

	n, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult)
	if err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}
	if n != 1 {
		t.Errorf("ReconcileStuckDrain() = %d, want 1", n)
	}
	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusCordoned || got.DrainTaskID != nil {
		t.Errorf("node = {%v, drain_task_id=%v}, want cordoned/nil", got.Status, got.DrainTaskID)
	}
}

// TestReconcileStuckDrainSkipsLiveDrain is the Fix Risk Gate guard test: a
// draining node whose drain task is running AND whose backing job is still live
// (pending, as enqueued) is a live drain in progress and MUST NOT be cordoned
// (cordoning it would abort the live drain). The backstop reports 0 fixed and
// leaves the node draining with its pointer intact. This is the negative case
// for the dead-job branch: a running task is skipped only while its job lives.
func TestReconcileStuckDrainSkipsLiveDrain(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node, taskID := startDrain(t, s, ctx, "drain-live")

	if _, err := s.UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}

	n, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult)
	if err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}
	if n != 0 {
		t.Errorf("ReconcileStuckDrain() = %d, want 0 (live drain must be skipped)", n)
	}
	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusDraining {
		t.Errorf("node status = %v, want draining (live drain untouched)", got.Status)
	}
	if got.DrainTaskID == nil || *got.DrainTaskID != taskID {
		t.Errorf("node DrainTaskID = %v, want %v (intact)", got.DrainTaskID, taskID)
	}
}

// drainJobKey rebuilds the queue key for a drain task's backing job (the
// zero-padded sequence under jobs/) so a test can kill the job out from under a
// running task. It mirrors the unexported jobKey in the package under test.
func drainJobKey(t *testing.T, jobID *int64) string {
	t.Helper()
	if jobID == nil {
		t.Fatal("drain task has no backing job id")
	}
	return etcd.Key("jobs", fmt.Sprintf("%020d", *jobID))
}

// TestReconcileStuckDrainCordonsRunningTaskWithDeadJob is the core of the
// dead-job backstop: a drain whose job exhausts its attempt budget leaves the
// task stuck in running while only the job is marked failed (here reproduced by
// deleting the job key). A task-status-only backstop would treat this as a live
// drain forever; reading the backing job reveals the saga is dead, so the
// backstop cordons the node and clears the dangling pointer. Reverting the
// dead-job branch (skip on any running task) makes this test fail: the node
// stays draining.
func TestReconcileStuckDrainCordonsRunningTaskWithDeadJob(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node, taskID := startDrain(t, s, ctx, "drain-dead-job")

	if _, err := s.UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	deleted, err := cli.Delete(ctx, drainJobKey(t, task.JobID))
	if err != nil {
		t.Fatalf("delete backing job: %v", err)
	}
	if !deleted {
		t.Fatalf("delete backing job: job %v not present", task.JobID)
	}

	n, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult)
	if err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}
	if n != 1 {
		t.Errorf("ReconcileStuckDrain() = %d, want 1 (running task with a dead job must be cordoned)", n)
	}
	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusCordoned {
		t.Errorf("node status = %v, want cordoned", got.Status)
	}
	if got.DrainTaskID != nil {
		t.Errorf("node DrainTaskID = %v, want nil", got.DrainTaskID)
	}
}

// reconciledDrainResult is the opaque task-result payload the drain backstop
// stamps on a wedged drain task it finalizes. The store treats it as opaque
// bytes (the drain handler owns the real DrainResult shape); the tests use a
// fixed payload and assert it round-trips onto the finalized task.
var reconciledDrainResult = []byte(`{"code":"drain_reconciled"}`)

// TestReconcileStuckDrainFinalizesWedgedTask locks the wedge fix: a draining
// node whose drain task is non-terminal (running) but whose backing job is dead
// must have that TASK finalized to a terminal state, not merely have the node
// cordoned. A left-running task is never reaped by retention (which keys on
// finished_at) and a cancel against it is a dead letter. The backstop must stamp
// finished_at and the reconciled result so retention reaps it.
func TestReconcileStuckDrainFinalizesWedgedTask(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	node, taskID := startDrain(t, s, ctx, "drain-finalize-wedged")

	if _, err := s.UpdateTaskRunning(ctx, taskID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if deleted, err := cli.Delete(ctx, drainJobKey(t, task.JobID)); err != nil || !deleted {
		t.Fatalf("delete backing job: deleted=%v err=%v", deleted, err)
	}

	if _, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult); err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}

	got, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID after reconcile: %v", err)
	}
	if got.Status != store.TaskStatusFailed {
		t.Errorf("wedged drain task status = %v, want failed (terminal, reapable)", got.Status)
	}
	if got.FinishedAt == nil {
		t.Error("wedged drain task finished_at = nil, want stamped (retention reaps by finished_at)")
	}
	if string(got.Result) != string(reconciledDrainResult) {
		t.Errorf("wedged drain task result = %q, want %q", got.Result, reconciledDrainResult)
	}
	n, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if n.Status != store.NodeStatusCordoned {
		t.Errorf("node status = %v, want cordoned", n.Status)
	}
}

// TestReconcileStuckDrainDoesNotClobberTerminalTask pins the fail-toward-inaction
// guard: if a drain task already reached a terminal status (its saga recorded the
// outcome) while the node was left draining (a torn crash between task-finalize
// and node-flip), the backstop cordons the node but must NOT clobber the recorded
// terminal result. Finalizing a settled success to failed would corrupt the
// operator-visible outcome.
func TestReconcileStuckDrainDoesNotClobberTerminalTask(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node, taskID := startDrain(t, s, ctx, "drain-no-clobber")

	settled := []byte(`{"code":"","migrated":1,"remaining":0}`)
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID: taskID, Status: store.TaskStatusSuccess, Result: settled,
	}); err != nil {
		t.Fatalf("UpdateTaskFinalized: %v", err)
	}

	if _, err := s.ReconcileStuckDrain(ctx, reconciledDrainResult); err != nil {
		t.Fatalf("ReconcileStuckDrain: %v", err)
	}

	got, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID after reconcile: %v", err)
	}
	if got.Status != store.TaskStatusSuccess {
		t.Errorf("settled drain task status = %v, want success (must not be clobbered)", got.Status)
	}
	if string(got.Result) != string(settled) {
		t.Errorf("settled drain task result = %q, want %q (must not be clobbered)", got.Result, settled)
	}
	n, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if n.Status != store.NodeStatusCordoned {
		t.Errorf("node status = %v, want cordoned", n.Status)
	}
}

// TestCordonRefusesGoneNode pins the resurrection guard: the reaper can mark a
// node terminally gone after the cordon handler's status check. setNodeCordon
// re-reads fresh and must refuse rather than resurrect the gone node to cordoned
// (which bypasses the readmit health fence). Mirrors ReadmitNode's fresh-status
// recheck - the ModRevision CAS alone cannot catch a status change that landed
// before this method's own fresh read. Both cordon and uncordon must refuse.
func TestCordonRefusesGoneNode(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	node := seedNodeWithStatus(t, s, ctx, "cordon-gone", store.NodeStatusGone)

	if _, err := s.CordonNode(ctx, node.ID); !errors.Is(err, store.ErrConcurrentUpdate) {
		t.Errorf("CordonNode(gone) = %v, want ErrConcurrentUpdate (must not resurrect a gone node)", err)
	}
	if _, err := s.UncordonNode(ctx, node.ID); !errors.Is(err, store.ErrConcurrentUpdate) {
		t.Errorf("UncordonNode(gone) = %v, want ErrConcurrentUpdate", err)
	}
	got, err := s.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Status != store.NodeStatusGone {
		t.Errorf("node status = %v, want gone (cordon/uncordon must not resurrect a gone node)", got.Status)
	}
}

// TestCordonRefusesToClobberDrainingNode pins the seam between the cordon/
// uncordon store writers and an active drain. The handler validates the source
// status before calling CordonNode/UncordonNode, but that is a TOCTOU: a drain
// can flip the node to draining in the window between the handler's read and the
// store write. setNodeCordon must refuse such a write rather than blind-put
// cordoned over the drain - which would drop the drain_task_id and strand the
// saga (the stuck-drain backstop only scans draining nodes). Both CordonNode and
// UncordonNode must return store.ErrConcurrentUpdate and leave the node draining
// with its drain_task_id intact.
func TestCordonRefusesToClobberDrainingNode(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func(context.Context, uuid.UUID) (store.Node, error)
	}{
		{name: "cordon", call: s.CordonNode},
		{name: "uncordon", call: s.UncordonNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, taskID := startDrain(t, s, ctx, "clobber-"+tc.name)

			_, err := tc.call(ctx, node.ID)
			if !errors.Is(err, store.ErrConcurrentUpdate) {
				t.Errorf("%s(draining node) = %v, want store.ErrConcurrentUpdate", tc.name, err)
			}

			got, err := s.NodeByID(ctx, node.ID)
			if err != nil {
				t.Fatalf("NodeByID: %v", err)
			}
			if got.Status != store.NodeStatusDraining {
				t.Errorf("node status = %v, want draining (drain must not be clobbered)", got.Status)
			}
			if got.DrainTaskID == nil || *got.DrainTaskID != taskID {
				t.Errorf("node DrainTaskID = %v, want %v (drain pointer must survive)", got.DrainTaskID, taskID)
			}
		})
	}
}
