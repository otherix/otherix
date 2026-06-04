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
	nodeErr error
	// templateChecksum is the binary checksum TemplateByID reports; nil models a
	// compute-mode template (empty checksum). A back-prop-modelling ensurer sets
	// it during EnsureImageOnPool so the reload picks it up.
	templateChecksum []byte

	finalizedFailed    bool
	finalizedError     []byte // the error envelope of the last failed finalize
	projectedCreate    bool
	projectedLifecycle bool
}

func (s *createLifecycleWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) error {
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

func (s *createLifecycleWorkerStoreStub) TemplateByID(_ context.Context, id uuid.UUID) (store.Template, error) {
	return store.Template{ID: id, ImageChecksumSha256: s.templateChecksum}, nil
}

func (s *createLifecycleWorkerStoreStub) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	return store.StoragePool{}, nil
}

func (s *createLifecycleWorkerStoreStub) ProjectVMCreateSuccess(context.Context, store.UpsertVMRuntimeParams, uuid.UUID, store.UpdateTaskFinalizedParams) error {
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

// spyImageEnsurer records whether (and in what order) EnsureImageOnPool was
// called and returns a canned error. order is a shared monotonic counter so a
// paired spyCreateExecutor can assert the ensure ran strictly before the agent
// create.
type spyImageEnsurer struct {
	order *int
	at    int
	err   error

	called bool
}

func (e *spyImageEnsurer) EnsureImageOnPool(context.Context, store.Template, store.StoragePool, store.Node) error {
	e.called = true
	if e.order != nil {
		*e.order++
		e.at = *e.order
	}
	return e.err
}

// spyCreateExecutor records whether the agent create was attempted, and at
// which order marker (so a test can assert the ensurer ran first).
type spyCreateExecutor struct {
	order *int
	at    int
	err   error

	called      bool
	gotTemplate store.Template // the template the worker handed the executor
}

func (e *spyCreateExecutor) Execute(_ context.Context, args CreateArgs) (CreateResult, error) {
	e.called = true
	e.gotTemplate = args.Template
	if e.order != nil {
		*e.order++
		e.at = *e.order
	}
	return CreateResult{}, e.err
}

// backpropImageEnsurer models the inline-import path that back-propagates the
// agent-computed checksum onto a compute-mode template: onEnsure runs during
// EnsureImageOnPool (e.g. to set the store stub's templateChecksum), so a
// subsequent template reload observes the populated checksum.
type backpropImageEnsurer struct {
	called   bool
	onEnsure func()
}

func (e *backpropImageEnsurer) EnsureImageOnPool(context.Context, store.Template, store.StoragePool, store.Node) error {
	e.called = true
	if e.onEnsure != nil {
		e.onEnsure()
	}
	return nil
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
			ensurer := &spyImageEnsurer{}

			err := runCreate(context.Background(), st, exec, ensurer, discardLog(),
				VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, staleGrace)

			if (err != nil) != tc.wantErr {
				t.Fatalf("runCreate err = %v, wantErr %v", err, tc.wantErr)
			}
			if exec.called != tc.wantCalled {
				t.Errorf("agent create attempted = %v, want %v", exec.called, tc.wantCalled)
			}
			if !tc.wantCalled {
				// Terminally dead: the ensure pre-phase is after the dead
				// check, so neither the ensurer nor the executor runs.
				if ensurer.called {
					t.Errorf("EnsureImageOnPool called on dead node; the ensure pre-phase must come after the dead-node check")
				}
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

	err := runCreate(context.Background(), st, exec, &spyImageEnsurer{}, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err == nil {
		t.Fatalf("runCreate = nil, want non-nil (retryable exec error)")
	}
	if !exec.called {
		t.Errorf("agent create not attempted on live node")
	}
}

// TestRunCreateEnsuresImageBeforeAgent pins the B1 ordering invariant: on a
// live ready node, runCreate materializes the template image on the pool
// (EnsureImageOnPool) strictly BEFORE dispatching the agent create, then
// projects success. A vm-create on a cold pool no longer hard-fails
// ErrTemplateMissing - the image is imported inline first.
func TestRunCreateEnsuresImageBeforeAgent(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID}}

	var seq int
	ensurer := &spyImageEnsurer{order: &seq}
	exec := &spyCreateExecutor{order: &seq}

	err := runCreate(context.Background(), st, exec, ensurer, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if !ensurer.called {
		t.Fatalf("EnsureImageOnPool not called on live node")
	}
	if !exec.called {
		t.Fatalf("agent create not attempted on live node")
	}
	if ensurer.at == 0 || exec.at == 0 || ensurer.at >= exec.at {
		t.Errorf("ensure/create order = (%d, %d), want ensure strictly before create", ensurer.at, exec.at)
	}
	if !st.projectedCreate {
		t.Errorf("ProjectVMCreateSuccess not called; a successful ensure+create must project success")
	}
}

// TestRunCreateEnsurerErrorRetryable pins the ensure-failure contract: when
// EnsureImageOnPool fails (a transient import failure - bad URL, checksum
// mismatch, disk full), runCreate finalizes the task FAILED, returns a non-nil
// (retryable) cause so the dispatcher retries the inline import, NEVER calls
// the agent create, and NEVER projects success.
func TestRunCreateEnsurerErrorRetryable(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID}}
	ensurer := &spyImageEnsurer{err: errors.New("disk full")}
	exec := &spyCreateExecutor{}

	err := runCreate(context.Background(), st, exec, ensurer, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err == nil {
		t.Fatalf("runCreate = nil, want non-nil (retryable ensure error)")
	}
	if !ensurer.called {
		t.Errorf("EnsureImageOnPool not attempted on live node")
	}
	if exec.called {
		t.Errorf("agent create attempted after ensure failure; the create must short-circuit")
	}
	if !st.finalizedFailed {
		t.Errorf("task not finalized failed after ensure failure")
	}
	if st.projectedCreate {
		t.Errorf("ProjectVMCreateSuccess called after ensure failure; a create that did not happen must NOT project success")
	}
	// A non-agent ensure failure (disk full at projection, decode error) carries
	// the fallback code: classifyVMError passes agent envelope codes through, but
	// a plain error falls through to errCodeVMImageUnavailable.
	if got := finalizedErrorCode(t, st.finalizedError); got != errCodeVMImageUnavailable {
		t.Errorf("ensure-failure task error code = %q, want %q", got, errCodeVMImageUnavailable)
	}
}

// finalizedErrorCode decodes the {code,message} envelope a failed finalize wrote.
func finalizedErrorCode(t *testing.T, envelope []byte) string {
	t.Helper()
	if len(envelope) == 0 {
		t.Fatalf("no error envelope recorded")
	}
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(envelope, &e); err != nil {
		t.Fatalf("decode error envelope %q: %v", envelope, err)
	}
	return e.Code
}

// TestRunCreateReloadsTemplateChecksumAfterEnsure pins the B1 seam fix: a cold
// create on a compute-mode template (empty checksum) must reload the template
// AFTER the inline import back-propagates the agent-computed checksum, so the
// agent create carries the 64-char template_checksum it requires. Without the
// reload the worker hands the executor the stale empty-checksum template and the
// agent rejects the create with invalid_spec on the exact cold-pool path B1
// exists to make seamless.
func TestRunCreateReloadsTemplateChecksumAfterEnsure(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	templateID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	// templateChecksum starts nil: a compute-mode template the import will fill.
	st := &createLifecycleWorkerStoreStub{vm: store.VM{ID: vmID}, node: node, disk: store.VMDisk{VmID: vmID}}
	computed := bytes.Repeat([]byte{0xAB}, 32) // 32 bytes -> 64 hex chars
	ensurer := &backpropImageEnsurer{onEnsure: func() { st.templateChecksum = computed }}
	exec := &spyCreateExecutor{}

	err := runCreate(context.Background(), st, exec, ensurer, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID, TemplateID: templateID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if !ensurer.called || !exec.called {
		t.Fatalf("ensurer.called=%v exec.called=%v, want both true", ensurer.called, exec.called)
	}
	got := hex.EncodeToString(exec.gotTemplate.ImageChecksumSha256)
	want := hex.EncodeToString(computed)
	if got != want {
		t.Errorf("create executor template checksum = %q, want %q (runCreate must reload the template after the inline import back-propagates the checksum)", got, want)
	}
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
