// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestLifecycleKinds(t *testing.T) {
	want := []LifecycleKind{
		{Kind: "vm.start", Op: "start", DesiredPhase: store.VmDesiredPhaseRunning, RuntimePhase: store.VmPhaseRunning, FailureCode: errCodeVMStartFailed},
		{Kind: "vm.stop", Op: "stop", DesiredPhase: store.VmDesiredPhaseStopped, RuntimePhase: store.VmPhaseStopped, FailureCode: errCodeVMStopFailed},
		{Kind: "vm.poweroff", Op: "poweroff", DesiredPhase: store.VmDesiredPhaseStopped, RuntimePhase: store.VmPhaseStopped, FailureCode: errCodeVMPoweroffFailed},
		{Kind: "vm.reboot", Op: "reboot", DesiredPhase: store.VMDesiredPhase(""), RuntimePhase: store.VmPhaseRunning, FailureCode: errCodeVMRebootFailed},
	}
	got := LifecycleKinds()
	if len(got) != len(want) {
		t.Fatalf("LifecycleKinds() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LifecycleKinds()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// deleteWorkerStoreStub is the minimal WorkerStore the runDelete tests
// need: it records the projected delete and serves canned entity reads.
// Only the methods runDelete touches are implemented; the rest panic so
// an accidental call surfaces loudly.
type deleteWorkerStoreStub struct {
	vm   store.VM
	node store.Node
	// nodeErr is returned by NodeByID; store.ErrNotFound models a
	// force-deleted owning node.
	nodeErr error

	projectedDelete bool
}

func (s *deleteWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) error { return nil }

func (s *deleteWorkerStoreStub) UpdateTaskFinalized(context.Context, store.UpdateTaskFinalizedParams) error {
	return nil
}

func (s *deleteWorkerStoreStub) UpdateTaskAgentTaskID(context.Context, store.UpdateTaskAgentTaskIDParams) error {
	return nil
}

func (s *deleteWorkerStoreStub) TaskByID(context.Context, uuid.UUID) (store.Task, error) {
	return store.Task{}, nil
}

func (s *deleteWorkerStoreStub) VMByID(context.Context, uuid.UUID) (store.VM, error) {
	return s.vm, nil
}

func (s *deleteWorkerStoreStub) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	if s.nodeErr != nil {
		return store.Node{}, s.nodeErr
	}
	return s.node, nil
}

func (s *deleteWorkerStoreStub) ProjectVMDeleteSuccess(context.Context, store.VM, store.UpdateTaskFinalizedParams) error {
	s.projectedDelete = true
	return nil
}

func (s *deleteWorkerStoreStub) ListVMDisksByVM(context.Context, uuid.UUID) ([]store.VMDisk, error) {
	panic("unexpected ListVMDisksByVM")
}

func (s *deleteWorkerStoreStub) ListVMNicsByVM(context.Context, uuid.UUID) ([]store.VMNic, error) {
	panic("unexpected ListVMNicsByVM")
}

func (s *deleteWorkerStoreStub) NetworkByID(context.Context, uuid.UUID) (store.Network, error) {
	panic("unexpected NetworkByID")
}

func (s *deleteWorkerStoreStub) TemplateByID(context.Context, uuid.UUID) (store.Template, error) {
	panic("unexpected TemplateByID")
}

func (s *deleteWorkerStoreStub) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	panic("unexpected StoragePoolByID")
}

func (s *deleteWorkerStoreStub) ProjectVMCreateSuccess(context.Context, store.UpsertVMRuntimeParams, uuid.UUID, store.UpdateTaskFinalizedParams) error {
	panic("unexpected ProjectVMCreateSuccess")
}

func (s *deleteWorkerStoreStub) ProjectVMLifecycleSuccess(context.Context, uuid.UUID, store.VMDesiredPhase, store.VMPhase, store.UpdateTaskFinalizedParams) error {
	panic("unexpected ProjectVMLifecycleSuccess")
}

// spyDeleteExecutor records whether the agent teardown was attempted and
// returns a canned outcome.
type spyDeleteExecutor struct {
	called bool
	err    error
}

func (e *spyDeleteExecutor) Execute(context.Context, DeleteArgs) (DeleteResult, error) {
	e.called = true
	if e.err != nil {
		return DeleteResult{}, e.err
	}
	return DeleteResult{}, nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunDelete_AgentTeardownByNodeState pins the dead-node tolerance
// contract: a 'gone' or force-deleted (missing) owning node skips the
// agent and projects the delete directly, but an 'unreachable' node -
// which may merely be partitioned with qemu still running - is given a
// best-effort agent teardown first. The delete only falls back to a
// direct projection when that teardown fails (the node is truly down).
func TestRunDelete_AgentTeardownByNodeState(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	baseVM := store.VM{ID: vmID, Name: "vm-x"}
	baseNode := store.Node{ID: nodeID, Name: "node-x", AdvertisedEndpoint: "https://node-x:8443"}

	tests := []struct {
		name       string
		nodeStatus store.NodeStatus
		nodeErr    error
		execErr    error
		wantCalled bool // agent teardown attempted
		wantErr    bool // runDelete returns an error
	}{
		{
			name:       "gone node skips agent and projects directly",
			nodeStatus: store.NodeStatusGone,
			wantCalled: false,
		},
		{
			name:       "missing node (force-deleted) skips agent and projects directly",
			nodeErr:    store.ErrNotFound,
			wantCalled: false,
		},
		{
			name:       "unreachable node attempts agent teardown and succeeds",
			nodeStatus: store.NodeStatusUnreachable,
			wantCalled: true,
		},
		{
			name:       "unreachable node falls back to direct projection when agent unreachable",
			nodeStatus: store.NodeStatusUnreachable,
			execErr:    errors.New("dial node-x: connection refused"),
			wantCalled: true,
		},
		{
			name:       "ready node goes through the agent",
			nodeStatus: store.NodeStatusReady,
			wantCalled: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := baseNode
			node.Status = tc.nodeStatus
			st := &deleteWorkerStoreStub{vm: baseVM, node: node, nodeErr: tc.nodeErr}
			exec := &spyDeleteExecutor{err: tc.execErr}

			err := runDelete(context.Background(), st, exec, discardLog(),
				VMDeleteArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID})

			if (err != nil) != tc.wantErr {
				t.Fatalf("runDelete err = %v, wantErr %v", err, tc.wantErr)
			}
			if exec.called != tc.wantCalled {
				t.Errorf("agent teardown attempted = %v, want %v", exec.called, tc.wantCalled)
			}
			if !st.projectedDelete {
				t.Errorf("ProjectVMDeleteSuccess not called; the delete must always converge to projected success")
			}
		})
	}
}
