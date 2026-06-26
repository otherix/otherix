// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
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
