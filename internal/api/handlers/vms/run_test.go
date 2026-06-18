// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
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
	// taskTerminal makes UpdateTaskRunning report the task already
	// committed-terminal (success/cancelled), so the handler must abort.
	taskTerminal bool

	projectedDelete    bool
	clearedAgentTaskID bool
}

func (s *deleteWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) (bool, error) {
	return s.taskTerminal, nil
}

func (s *deleteWorkerStoreStub) ClearTaskAgentTaskID(context.Context, uuid.UUID) error {
	s.clearedAgentTaskID = true
	return nil
}

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

func (s *deleteWorkerStoreStub) SnapshotByID(context.Context, uuid.UUID) (store.Snapshot, error) {
	panic("unexpected SnapshotByID")
}

func (s *deleteWorkerStoreStub) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	panic("unexpected StoragePoolByID")
}

func (s *deleteWorkerStoreStub) ProjectVMCreateSuccess(context.Context, store.UpsertVMRuntimeParams, store.UpdateTaskFinalizedParams, []byte) error {
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
	// recentHeartbeat keeps the existing live-node cases out of the
	// staleness arm: only node status drives terminal-deadness here.
	recentHeartbeat := time.Now().Add(-5 * time.Second)
	baseNode := store.Node{ID: nodeID, Name: "node-x", AdvertisedEndpoint: "https://node-x:8443", LastHeartbeatAt: &recentHeartbeat}

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

			err := runDelete(context.Background(), st, exec, discardLog(), 5*time.Minute,
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

// createLifecycleWorkerStoreStub is the minimal WorkerStore the
// runCreate / runLifecycle short-circuit tests need: it serves canned
// entity reads (vm, node, single disk) and records whether a failed
// finalize ran and whether a success projection ran. The agent-exec
// short-circuit on a terminally-dead node must reach NEITHER projection.
type createLifecycleWorkerStoreStub struct {
	vm      store.VM
	node    store.Node
	disk    store.VMDisk
	pool    store.StoragePool
	nodeErr error
	// taskTerminal makes UpdateTaskRunning report the task already
	// committed-terminal (success/cancelled), so the handler must abort.
	taskTerminal bool
	// snapshotByID, when set, serves the recreate-from-snapshot manifest read so
	// the SEAM test can assert the worker resolves args.SourceSnapshotID into the
	// agent request's source_snapshot before dispatch.
	snapshotByID func(id uuid.UUID) (store.Snapshot, error)

	finalizedFailed    bool
	finalizedError     []byte // the error envelope of the last failed finalize
	projectedCreate    bool
	projectedLifecycle bool
	clearedAgentTaskID bool
}

func (s *createLifecycleWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) (bool, error) {
	return s.taskTerminal, nil
}

func (s *createLifecycleWorkerStoreStub) ClearTaskAgentTaskID(context.Context, uuid.UUID) error {
	s.clearedAgentTaskID = true
	return nil
}

func (s *createLifecycleWorkerStoreStub) UpdateTaskFinalized(_ context.Context, arg store.UpdateTaskFinalizedParams) error {
	if arg.Status == store.TaskStatusFailed {
		s.finalizedFailed = true
		s.finalizedError = arg.Error
	}
	return nil
}

func (s *createLifecycleWorkerStoreStub) UpdateTaskAgentTaskID(context.Context, store.UpdateTaskAgentTaskIDParams) error {
	return nil
}

func (s *createLifecycleWorkerStoreStub) TaskByID(context.Context, uuid.UUID) (store.Task, error) {
	return store.Task{}, nil
}

func (s *createLifecycleWorkerStoreStub) VMByID(context.Context, uuid.UUID) (store.VM, error) {
	return s.vm, nil
}

func (s *createLifecycleWorkerStoreStub) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	if s.nodeErr != nil {
		return store.Node{}, s.nodeErr
	}
	return s.node, nil
}

func (s *createLifecycleWorkerStoreStub) ListVMDisksByVM(context.Context, uuid.UUID) ([]store.VMDisk, error) {
	return []store.VMDisk{s.disk}, nil
}

func (s *createLifecycleWorkerStoreStub) ListVMNicsByVM(context.Context, uuid.UUID) ([]store.VMNic, error) {
	return nil, nil
}

func (s *createLifecycleWorkerStoreStub) NetworkByID(context.Context, uuid.UUID) (store.Network, error) {
	panic("unexpected NetworkByID")
}

func (s *createLifecycleWorkerStoreStub) SnapshotByID(_ context.Context, id uuid.UUID) (store.Snapshot, error) {
	if s.snapshotByID != nil {
		return s.snapshotByID(id)
	}
	panic("unexpected SnapshotByID")
}

func (s *createLifecycleWorkerStoreStub) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	return s.pool, nil
}

func (s *createLifecycleWorkerStoreStub) ProjectVMCreateSuccess(context.Context, store.UpsertVMRuntimeParams, store.UpdateTaskFinalizedParams, []byte) error {
	s.projectedCreate = true
	return nil
}

func (s *createLifecycleWorkerStoreStub) ProjectVMDeleteSuccess(context.Context, store.VM, store.UpdateTaskFinalizedParams) error {
	panic("unexpected ProjectVMDeleteSuccess")
}

func (s *createLifecycleWorkerStoreStub) ProjectVMLifecycleSuccess(context.Context, uuid.UUID, store.VMDesiredPhase, store.VMPhase, store.UpdateTaskFinalizedParams) error {
	s.projectedLifecycle = true
	return nil
}

// spyCreateExecutor records whether the agent create was attempted, the image
// source the worker handed it, and a canned result/error to surface back.
type spyCreateExecutor struct {
	err    error
	result CreateResult

	called  bool
	gotArgs CreateArgs // the args the worker handed the executor
}

func (e *spyCreateExecutor) Execute(_ context.Context, args CreateArgs) (CreateResult, error) {
	e.called = true
	e.gotArgs = args
	return e.result, e.err
}

// spyLifecycleExecutor records whether the agent lifecycle op was attempted.
type spyLifecycleExecutor struct {
	called bool
	err    error
}

func (e *spyLifecycleExecutor) Execute(context.Context, string, LifecycleArgs) (LifecycleResult, error) {
	e.called = true
	return LifecycleResult{}, e.err
}

// TestRunCreateTerminallyDeadNode pins OOS-1 for the create path: a
// create against a terminally-dead owning node (gone / force-deleted /
// stale-heartbeat) must FAIL FAST and TERMINALLY - the executor is never
// called, the task is finalized FAILED (not success), no VM-create
// success is projected, and the handler returns NIL so the dispatcher
// COMPLETES the job instead of burning the retry budget. The live-node
// case is the revert-to-confirm: a ready node with a fresh heartbeat
// still reaches the agent.
func TestRunCreateTerminallyDeadNode(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	baseVM := store.VM{ID: vmID, Name: "vm-x"}
	disk := store.VMDisk{VmID: vmID, DeviceOrder: 0}
	staleGrace := 5 * time.Minute
	fresh := time.Now().Add(-5 * time.Second)
	stale := time.Now().Add(-10 * time.Minute)

	tests := []struct {
		name          string
		nodeStatus    store.NodeStatus
		nodeErr       error
		lastHeartbeat *time.Time
		wantCalled    bool // agent create attempted
		wantErr       bool // runCreate returns an error (retry)
	}{
		{
			name:          "gone node fails terminally, no agent, no projection",
			nodeStatus:    store.NodeStatusGone,
			lastHeartbeat: &fresh,
			wantCalled:    false,
			wantErr:       false,
		},
		{
			name:       "force-deleted node fails terminally, no agent, no projection",
			nodeErr:    store.ErrNotFound,
			wantCalled: false,
			wantErr:    false,
		},
		{
			name:          "stale-heartbeat node fails terminally, no agent, no projection",
			nodeStatus:    store.NodeStatusCordoned,
			lastHeartbeat: &stale,
			wantCalled:    false,
			wantErr:       false,
		},
		{
			name:          "live ready node reaches the agent",
			nodeStatus:    store.NodeStatusReady,
			lastHeartbeat: &fresh,
			wantCalled:    true,
			wantErr:       false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := store.Node{ID: nodeID, Name: "node-x", Status: tc.nodeStatus, LastHeartbeatAt: tc.lastHeartbeat}
			st := &createLifecycleWorkerStoreStub{vm: baseVM, node: node, disk: disk, nodeErr: tc.nodeErr}
			exec := &spyCreateExecutor{}

			err := runCreate(context.Background(), st, exec, discardLog(),
				VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, staleGrace)

			if (err != nil) != tc.wantErr {
				t.Fatalf("runCreate err = %v, wantErr %v", err, tc.wantErr)
			}
			if exec.called != tc.wantCalled {
				t.Errorf("agent create attempted = %v, want %v", exec.called, tc.wantCalled)
			}
			if !tc.wantCalled {
				// Terminally dead: must be a clean terminal failure, no success.
				if !st.finalizedFailed {
					t.Errorf("task not finalized failed on dead node")
				}
				if st.projectedCreate {
					t.Errorf("ProjectVMCreateSuccess called on dead node; a create that did not happen must NOT project success")
				}
			}
		})
	}
}

// TestRunCreateAbortsOnTerminalTask pins R2-M3 for the create path: a redelivery
// whose task is already committed-terminal (UpdateTaskRunning reports
// alreadyTerminal=true) completes WITHOUT contacting the agent - the executor is
// never called and no projection runs. The dispatcher then CompleteJob-deletes
// the job.
func TestRunCreateAbortsOnTerminalTask(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{
		vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID},
		taskTerminal: true,
	}
	exec := &spyCreateExecutor{}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate(terminal task) = %v, want nil", err)
	}
	if exec.called {
		t.Errorf("agent create attempted on a committed-terminal task; must abort without contacting the agent")
	}
	if st.projectedCreate {
		t.Errorf("ProjectVMCreateSuccess called on a committed-terminal task; the abort path must not project")
	}
}

// TestRunLifecycleAbortsOnTerminalTask pins R2-M3 for the lifecycle path: a
// redelivery of an already committed-terminal task returns nil without invoking
// the agent lifecycle executor or projecting.
func TestRunLifecycleAbortsOnTerminalTask(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, taskTerminal: true}
	exec := &spyLifecycleExecutor{}

	err := runLifecycle(context.Background(), st, exec, discardLog(), uuid.New(), vmID, nodeID,
		"start", store.VmDesiredPhaseRunning, store.VmPhaseRunning, errCodeVMStartFailed, 5*time.Minute)
	if err != nil {
		t.Fatalf("runLifecycle(terminal task) = %v, want nil", err)
	}
	if exec.called {
		t.Errorf("agent lifecycle attempted on a committed-terminal task; must abort")
	}
	if st.projectedLifecycle {
		t.Errorf("ProjectVMLifecycleSuccess called on a committed-terminal task; the abort path must not project")
	}
}

// TestRunDeleteAbortsOnTerminalTask pins R2-M3 for the delete path: a redelivery
// of an already committed-terminal task returns nil without invoking the agent
// delete executor or projecting.
func TestRunDeleteAbortsOnTerminalTask(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &deleteWorkerStoreStub{vm: store.VM{ID: vmID, Name: "vm-x"}, node: node, taskTerminal: true}
	exec := &spyDeleteExecutor{}

	err := runDelete(context.Background(), st, exec, discardLog(), 5*time.Minute,
		VMDeleteArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID})
	if err != nil {
		t.Fatalf("runDelete(terminal task) = %v, want nil", err)
	}
	if exec.called {
		t.Errorf("agent teardown attempted on a committed-terminal task; must abort")
	}
	if st.projectedDelete {
		t.Errorf("ProjectVMDeleteSuccess called on a committed-terminal task; the abort path must not project")
	}
}

// TestRunCreateExecErrorRetryable confirms failRun semantics are
// unchanged on the live path: an agent-exec error against a still-live
// node returns a NON-nil cause so the dispatcher retries (the failure
// may be transient).
func TestRunCreateExecErrorRetryable(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID}}
	exec := &spyCreateExecutor{err: errors.New("agent boom")}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err == nil {
		t.Fatalf("runCreate = nil, want non-nil (retryable exec error)")
	}
	if !exec.called {
		t.Errorf("agent create not attempted on live node")
	}
}

// TestRunCreateClearsAgentTaskIDOn404 pins R2-M2 for the create path: when the
// agent task vanished (poll 404, e.g. the agent restarted and lost its in-memory
// task store), the worker clears the persisted agent_task_id so the redelivery
// re-POSTs and the agent's durable dedup reconciles. It still returns a retryable
// error (failRun semantics).
func TestRunCreateClearsAgentTaskIDOn404(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID}}
	exec := &spyCreateExecutor{err: &agentclient.AgentError{Status: 404, Code: "not_found"}}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err == nil {
		t.Fatalf("runCreate = nil, want non-nil (retryable after clear)")
	}
	if !st.clearedAgentTaskID {
		t.Errorf("agent_task_id not cleared on a 404; the redelivery cannot re-POST")
	}
}

// TestRunCreateDoesNotClearOnNon404 confirms the clear is narrow: a non-404 agent
// error must NOT clear the persisted agent_task_id.
func TestRunCreateDoesNotClearOnNon404(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID}}
	exec := &spyCreateExecutor{err: &agentclient.AgentError{Status: 500, Code: "internal"}}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err == nil {
		t.Fatalf("runCreate = nil, want non-nil (retryable)")
	}
	if st.clearedAgentTaskID {
		t.Errorf("agent_task_id cleared on a non-404 error; the clear must be 404-only")
	}
}

// TestRunDeleteClearsAgentTaskIDOn404 mirrors the create case for delete on a
// live (ready) node.
func TestRunDeleteClearsAgentTaskIDOn404(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, AdvertisedEndpoint: "https://node-x:8443", LastHeartbeatAt: &fresh}
	st := &deleteWorkerStoreStub{vm: store.VM{ID: vmID, Name: "vm-x"}, node: node}
	exec := &spyDeleteExecutor{err: &agentclient.AgentError{Status: 404, Code: "not_found"}}

	err := runDelete(context.Background(), st, exec, discardLog(), 5*time.Minute,
		VMDeleteArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID})
	if err == nil {
		t.Fatalf("runDelete = nil, want non-nil (retryable after clear)")
	}
	if !st.clearedAgentTaskID {
		t.Errorf("agent_task_id not cleared on a 404; the redelivery cannot re-POST")
	}
}

// TestRunDeleteDoesNotClearOnNon404: a non-404 delete error on a live node must
// not clear the persisted agent_task_id.
func TestRunDeleteDoesNotClearOnNon404(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, AdvertisedEndpoint: "https://node-x:8443", LastHeartbeatAt: &fresh}
	st := &deleteWorkerStoreStub{vm: store.VM{ID: vmID, Name: "vm-x"}, node: node}
	exec := &spyDeleteExecutor{err: &agentclient.AgentError{Status: 500, Code: "internal"}}

	err := runDelete(context.Background(), st, exec, discardLog(), 5*time.Minute,
		VMDeleteArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID})
	if err == nil {
		t.Fatalf("runDelete = nil, want non-nil (retryable)")
	}
	if st.clearedAgentTaskID {
		t.Errorf("agent_task_id cleared on a non-404 error; the clear must be 404-only")
	}
}

// TestRunCreateHandsImageSourceFromVMRow pins the create seam: the worker reads
// the image source off the self-describing VM + disk rows and hands it to the
// executor (image_url + hex digest + format + disk size), then projects success.
// There is no template lookup and no CP-side image ensurer.
func TestRunCreateHandsImageSourceFromVMRow(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	sha := bytes.Repeat([]byte{0xAB}, 32) // 32 bytes -> 64 hex chars
	vm := store.VM{
		ID:          vmID,
		ImageURL:    "https://example.test/img.qcow2",
		ImageSHA256: sha,
		ImageFormat: store.ImageFormatQcow2,
	}
	disk := store.VMDisk{VmID: vmID, SizeGib: 20}
	st := &createLifecycleWorkerStoreStub{vm: vm, node: node, disk: disk}
	exec := &spyCreateExecutor{}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if !exec.called {
		t.Fatalf("agent create not attempted on live node")
	}
	if exec.gotArgs.ImageURL != vm.ImageURL {
		t.Errorf("CreateArgs.ImageURL = %q, want %q", exec.gotArgs.ImageURL, vm.ImageURL)
	}
	if got, want := exec.gotArgs.ImageSHA256, hex.EncodeToString(sha); got != want {
		t.Errorf("CreateArgs.ImageSHA256 = %q, want %q", got, want)
	}
	if exec.gotArgs.Format != string(store.ImageFormatQcow2) {
		t.Errorf("CreateArgs.Format = %q, want %q", exec.gotArgs.Format, store.ImageFormatQcow2)
	}
	if exec.gotArgs.DiskGiB != 20 {
		t.Errorf("CreateArgs.DiskGiB = %d, want 20", exec.gotArgs.DiskGiB)
	}
	if !st.projectedCreate {
		t.Errorf("ProjectVMCreateSuccess not called; a successful create must project success")
	}
}

// TestRunCreateSurfacesAgentResolvedResult pins the success-projection seam: the
// resolved content digest and disk sizes the agent computed (returned by the
// executor in CreateResult) are echoed into the task result the worker
// finalizes.
func TestRunCreateSurfacesAgentResolvedResult(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &captureResultStoreStub{createLifecycleWorkerStoreStub: createLifecycleWorkerStoreStub{
		vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID},
	}}
	exec := &spyCreateExecutor{result: CreateResult{
		VMID:             vmID.String(),
		ImageSHA256:      "deadbeef",
		DiskSizeBytes:    123,
		VirtualSizeBytes: 456,
	}}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	var got CreateResult
	if err := json.Unmarshal(st.finalResult, &got); err != nil {
		t.Fatalf("decode task result %q: %v", st.finalResult, err)
	}
	want := CreateResult{VMID: vmID.String(), ImageSHA256: "deadbeef", DiskSizeBytes: 123, VirtualSizeBytes: 456}
	if got != want {
		t.Errorf("task result = %+v, want %+v", got, want)
	}
	// The hex digest the agent resolved is decoded and threaded to the
	// projection so it can stamp the VM row (compute-mode createsurface the
	// digest on the VM view).
	if gotDigest := hex.EncodeToString(st.finalDigest); gotDigest != "deadbeef" {
		t.Errorf("projected image digest = %q, want %q", gotDigest, "deadbeef")
	}
}

// TestRunCreateResolvesSourceSnapshotSeam pins the worker->agent SEAM for
// recreate-from-snapshot: when VMCreateArgs.SourceSnapshotID is set, the worker
// loads the snapshot manifest and hands the agent the resolved, index-ordered
// blob digests (the agent has no CP store access, so it can only materialize
// from what the worker resolves). A regression where the worker forgets to
// populate source_snapshot would leave CreateArgs.SourceSnapshot nil and silently
// fall back to an (empty) image - this asserts the digests travel.
func TestRunCreateResolvesSourceSnapshotSeam(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	snapID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	// Manifest disks intentionally out of index order so the test proves the
	// worker sorts them into virtio<i> order before sending.
	snap := store.Snapshot{
		ID: snapID,
		Disks: []store.SnapshotDisk{
			{Index: 1, Device: "virtio1", SHA256: "bbbb", SizeBytes: 2},
			{Index: 0, Device: "virtio0", SHA256: "aaaa", SizeBytes: 1},
		},
	}
	st := &createLifecycleWorkerStoreStub{
		vm:           store.VM{ID: vmID, SourceSnapshotID: &snapID},
		node:         node,
		disk:         store.VMDisk{VmID: vmID},
		pool:         store.StoragePool{Name: "pool-a"},
		snapshotByID: func(uuid.UUID) (store.Snapshot, error) { return snap, nil },
	}
	exec := &spyCreateExecutor{}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID, SourceSnapshotID: &snapID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if !exec.called {
		t.Fatalf("agent create not attempted")
	}
	got := exec.gotArgs.SourceSnapshot
	if got == nil {
		t.Fatalf("CreateArgs.SourceSnapshot = nil, want resolved snapshot blobs (SEAM regression)")
	}
	if got.Pool != "pool-a" {
		t.Errorf("SourceSnapshot.Pool = %q, want pool-a", got.Pool)
	}
	want := []agentclient.VMCreateSourceSnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aaaa"},
		{Index: 1, Device: "virtio1", SHA256: "bbbb"},
	}
	if diff := cmp.Diff(want, got.Disks); diff != "" {
		t.Errorf("SourceSnapshot.Disks mismatch (-want +got):\n%s", diff)
	}
}

// TestRunCreateFailsWhenSourceSnapshotMissing pins the fail-closed side of the
// SEAM: a recreate whose snapshot id cannot be resolved must FAIL the create
// rather than silently dispatch a sourceless (empty-image) create to the agent.
func TestRunCreateFailsWhenSourceSnapshotMissing(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	snapID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{
		vm:           store.VM{ID: vmID, SourceSnapshotID: &snapID},
		node:         node,
		disk:         store.VMDisk{VmID: vmID},
		pool:         store.StoragePool{Name: "pool-a"},
		snapshotByID: func(uuid.UUID) (store.Snapshot, error) { return store.Snapshot{}, store.ErrNotFound },
	}
	exec := &spyCreateExecutor{}

	err := runCreate(context.Background(), st, exec, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID, SourceSnapshotID: &snapID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil (failure is finalized, not propagated)", err)
	}
	if exec.called {
		t.Errorf("executor was called despite unresolvable snapshot; recreate must fail closed")
	}
	if !st.finalizedFailed {
		t.Errorf("create not finalized failed on unresolvable snapshot")
	}
}

// captureResultStoreStub captures the result bytes and the resolved image
// digest the create projection receives, so a test can assert the
// agent-resolved fields surfaced into the task result and the VM-row digest
// stamp.
type captureResultStoreStub struct {
	createLifecycleWorkerStoreStub
	finalResult []byte
	finalDigest []byte
}

func (s *captureResultStoreStub) ProjectVMCreateSuccess(_ context.Context, _ store.UpsertVMRuntimeParams, fin store.UpdateTaskFinalizedParams, imageSHA256 []byte) error {
	s.finalResult = fin.Result
	s.finalDigest = imageSHA256
	return nil
}

// TestRunLifecycleTerminallyDeadNode is the lifecycle analogue of
// TestRunCreateTerminallyDeadNode: a start/stop on a terminally-dead
// owning node fails fast and terminally (no agent, no success
// projection, returns nil). A start/stop on a dead node did NOT happen,
// so projecting lifecycle success would lie about runtime state.
func TestRunLifecycleTerminallyDeadNode(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	baseVM := store.VM{ID: vmID, Name: "vm-x"}
	staleGrace := 5 * time.Minute
	fresh := time.Now().Add(-5 * time.Second)
	stale := time.Now().Add(-10 * time.Minute)

	tests := []struct {
		name          string
		nodeStatus    store.NodeStatus
		nodeErr       error
		lastHeartbeat *time.Time
		wantCalled    bool
		wantErr       bool
	}{
		{
			name:          "gone node fails terminally, no agent, no projection",
			nodeStatus:    store.NodeStatusGone,
			lastHeartbeat: &fresh,
			wantCalled:    false,
			wantErr:       false,
		},
		{
			name:       "force-deleted node fails terminally, no agent, no projection",
			nodeErr:    store.ErrNotFound,
			wantCalled: false,
			wantErr:    false,
		},
		{
			name:          "stale-heartbeat node fails terminally, no agent, no projection",
			nodeStatus:    store.NodeStatusCordoned,
			lastHeartbeat: &stale,
			wantCalled:    false,
			wantErr:       false,
		},
		{
			name:          "live ready node reaches the agent",
			nodeStatus:    store.NodeStatusReady,
			lastHeartbeat: &fresh,
			wantCalled:    true,
			wantErr:       false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := store.Node{ID: nodeID, Name: "node-x", Status: tc.nodeStatus, LastHeartbeatAt: tc.lastHeartbeat}
			st := &createLifecycleWorkerStoreStub{vm: baseVM, node: node, nodeErr: tc.nodeErr}
			exec := &spyLifecycleExecutor{}

			err := runLifecycle(context.Background(), st, exec, discardLog(),
				uuid.New(), vmID, nodeID, "stop", store.VmDesiredPhaseStopped, store.VmPhaseStopped, errCodeVMStopFailed, staleGrace)

			if (err != nil) != tc.wantErr {
				t.Fatalf("runLifecycle err = %v, wantErr %v", err, tc.wantErr)
			}
			if exec.called != tc.wantCalled {
				t.Errorf("agent lifecycle attempted = %v, want %v", exec.called, tc.wantCalled)
			}
			if !tc.wantCalled {
				if !st.finalizedFailed {
					t.Errorf("task not finalized failed on dead node")
				}
				if st.projectedLifecycle {
					t.Errorf("ProjectVMLifecycleSuccess called on dead node; an op that did not happen must NOT project success")
				}
			}
		})
	}
}

// TestRunLifecycleExecErrorRetryable confirms failRun semantics are
// unchanged on the live path: an agent-exec error against a still-live
// node returns a NON-nil cause so the dispatcher retries.
func TestRunLifecycleExecErrorRetryable(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node}
	exec := &spyLifecycleExecutor{err: errors.New("agent boom")}

	err := runLifecycle(context.Background(), st, exec, discardLog(),
		uuid.New(), vmID, nodeID, "stop", store.VmDesiredPhaseStopped, store.VmPhaseStopped, errCodeVMStopFailed, 5*time.Minute)
	if err == nil {
		t.Fatalf("runLifecycle = nil, want non-nil (retryable exec error)")
	}
	if !exec.called {
		t.Errorf("agent lifecycle not attempted on live node")
	}
}

// TestRunDeleteCordonedStaleNode pins the heartbeat-staleness arm: a
// cordoned (or draining) node that dies never advances to 'gone' (the
// reaper only flips ready/pending). Without the staleness arm such a
// VM delete would failRun forever and its network would stay
// undeletable. A node unseen for longer than staleGrace is therefore
// terminally dead regardless of status: the delete projects directly
// with no agent call. The revert-to-confirm case proves the gate has
// teeth - a cordoned node with a RECENT heartbeat still takes the
// live-node branch and calls the agent once.
func TestRunDeleteCordonedStaleNode(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	baseVM := store.VM{ID: vmID, Name: "vm-x"}

	staleGrace := 5 * time.Minute

	tests := []struct {
		name          string
		lastHeartbeat time.Time
		wantCalled    bool // agent teardown attempted
	}{
		{
			name:          "cordoned node stale beyond grace projects directly",
			lastHeartbeat: time.Now().Add(-10 * time.Minute),
			wantCalled:    false,
		},
		{
			name:          "cordoned node with recent heartbeat still goes through the agent",
			lastHeartbeat: time.Now().Add(-5 * time.Second),
			wantCalled:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lastHeartbeat := tc.lastHeartbeat
			node := store.Node{
				ID:                 nodeID,
				Name:               "node-x",
				AdvertisedEndpoint: "https://node-x:8443",
				Status:             store.NodeStatusCordoned,
				LastHeartbeatAt:    &lastHeartbeat,
			}
			st := &deleteWorkerStoreStub{vm: baseVM, node: node}
			exec := &spyDeleteExecutor{}

			err := runDelete(context.Background(), st, exec, discardLog(), staleGrace,
				VMDeleteArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID})
			if err != nil {
				t.Fatalf("runDelete err = %v, want nil", err)
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
