// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// snapWorkerStoreStub is the WorkerStore double for the snapshot worker tests. It
// records the calls the seam tests assert on (the manifest projection inputs, the
// finalize status, the dereference request) and drives the redelivery branch via
// taskTerminal. Unset reads return benign zero rows so a test only needs to set
// the fields its assertion touches.
type snapWorkerStoreStub struct {
	taskTerminal bool

	snapshot store.Snapshot
	vm       store.VM
	runtime  store.VMRuntime
	node     store.Node

	// recorded create-path calls
	manifestDisks   []store.SnapshotDisk
	manifestVMState store.VMStateAtSnapshot
	manifestApplied bool

	// recorded delete-path calls
	dereferenceDigests []string
	orphanedReturn     []string

	// recorded terminal task outcome
	finalizedStatus store.TaskStatus
	finalized       bool
}

func (s *snapWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) (bool, error) {
	return s.taskTerminal, nil
}

func (s *snapWorkerStoreStub) UpdateTaskFinalized(_ context.Context, arg store.UpdateTaskFinalizedParams) error {
	s.finalized = true
	s.finalizedStatus = arg.Status
	return nil
}

func (s *snapWorkerStoreStub) SnapshotByID(context.Context, uuid.UUID) (store.Snapshot, error) {
	return s.snapshot, nil
}

func (s *snapWorkerStoreStub) SnapshotByIDIncludingDeleted(context.Context, uuid.UUID) (store.Snapshot, error) {
	return s.snapshot, nil
}

func (s *snapWorkerStoreStub) VMByID(context.Context, uuid.UUID) (store.VM, error) {
	return s.vm, nil
}

func (s *snapWorkerStoreStub) VMRuntimeByID(context.Context, uuid.UUID) (store.VMRuntime, error) {
	return s.runtime, nil
}

func (s *snapWorkerStoreStub) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	return s.node, nil
}

func (s *snapWorkerStoreStub) SnapshotManifestApplied(_ context.Context, _ uuid.UUID, disks []store.SnapshotDisk, vmState store.VMStateAtSnapshot) error {
	s.manifestApplied = true
	s.manifestDisks = disks
	s.manifestVMState = vmState
	return nil
}

func (s *snapWorkerStoreStub) DereferenceSnapshotBlobs(_ context.Context, _ uuid.UUID, digests []string) ([]string, error) {
	s.dereferenceDigests = digests
	return s.orphanedReturn, nil
}

// fakeSnapshotExecutor is the SnapshotExecutor double. createResult / createErr
// drive Create; deleteBlobs records the orphaned set handed to the agent so the
// fail-closed-GC test can assert ONLY orphaned digests are passed.
type fakeSnapshotExecutor struct {
	createCalled bool
	createResult CreateExecResult
	createErr    error

	deleteCalled bool
	deleteBlobs  []string
	deleteErr    error
}

func (e *fakeSnapshotExecutor) Create(context.Context, CreateExecArgs) (CreateExecResult, error) {
	e.createCalled = true
	return e.createResult, e.createErr
}

func (e *fakeSnapshotExecutor) Delete(_ context.Context, a DeleteExecArgs) error {
	e.deleteCalled = true
	e.deleteBlobs = a.OrphanedBlobs
	return e.deleteErr
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runningRuntimeStore() *snapWorkerStoreStub {
	nodeID := uuid.New()
	return &snapWorkerStoreStub{
		snapshot: store.Snapshot{ID: uuid.New(), VmID: uuid.New(), Name: "daily"},
		vm:       store.VM{Name: "myvm"},
		runtime:  store.VMRuntime{CurrentNodeID: &nodeID},
		node:     store.Node{ID: nodeID, AdvertisedEndpoint: "https://node-a:9443"},
	}
}

// TestRunSnapshotCreate_ProjectsManifestAndFinalizes drives the REAL handler
// factory (CreateHandler -> json args -> run body) so the test covers the unmarshal
// seam, not a pre-decoded direct call. On agent success the run must claim the task,
// project the agent-reported manifest (disks + vm_state_at_snapshot), and finalize
// the task SUCCESS.
func TestRunSnapshotCreate_ProjectsManifestAndFinalizes(t *testing.T) {
	st := runningRuntimeStore()
	exec := &fakeSnapshotExecutor{createResult: CreateExecResult{
		Disks: []store.SnapshotDisk{
			{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
			{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
		},
		VMStateAtSnapshot: store.VmStateAtSnapshotRunning,
	}}

	taskID := uuid.New()
	raw, _ := json.Marshal(SnapshotCreateArgs{TaskID: taskID, SnapshotID: st.snapshot.ID})
	if err := CreateHandler(st, exec, discardLogger())(context.Background(), raw); err != nil {
		t.Fatalf("CreateHandler run = %v, want nil", err)
	}

	if !exec.createCalled {
		t.Errorf("executor Create not called on a non-terminal task")
	}
	if !st.manifestApplied {
		t.Fatalf("SnapshotManifestApplied not called on agent success")
	}
	if len(st.manifestDisks) != 2 || st.manifestDisks[0].SHA256 != "aa" || st.manifestDisks[1].SHA256 != "bb" {
		t.Errorf("manifest disks = %+v, want the two agent-reported disks", st.manifestDisks)
	}
	if st.manifestVMState != store.VmStateAtSnapshotRunning {
		t.Errorf("manifest vm_state = %q, want running (authoritative agent report)", st.manifestVMState)
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusSuccess {
		t.Errorf("task finalized = (%v, %q), want (true, success)", st.finalized, st.finalizedStatus)
	}
}

// TestRunSnapshotCreate_ExecutorError_FailsTask: an executor error finalizes the
// task FAILED and the run returns the cause (so the dispatcher requeues against the
// attempt budget).
func TestRunSnapshotCreate_ExecutorError_FailsTask(t *testing.T) {
	st := runningRuntimeStore()
	cause := errors.New("agent blew up")
	exec := &fakeSnapshotExecutor{createErr: cause}

	err := runSnapshotCreate(context.Background(), st, exec, discardLogger(),
		SnapshotCreateArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID})
	if !errors.Is(err, cause) {
		t.Errorf("runSnapshotCreate err = %v, want the executor cause", err)
	}
	if st.manifestApplied {
		t.Errorf("SnapshotManifestApplied called despite executor error; must not project on failure")
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusFailed {
		t.Errorf("task finalized = (%v, %q), want (true, failed)", st.finalized, st.finalizedStatus)
	}
}

// TestRunSnapshotCreate_Redelivery_NoAgentCall: a redelivered/cancelled task whose
// UpdateTaskRunning reports alreadyTerminal must NOT re-POST to the agent; the run
// returns nil so the dispatcher CompleteJob-deletes the job.
func TestRunSnapshotCreate_Redelivery_NoAgentCall(t *testing.T) {
	st := runningRuntimeStore()
	st.taskTerminal = true
	exec := &fakeSnapshotExecutor{}

	err := runSnapshotCreate(context.Background(), st, exec, discardLogger(),
		SnapshotCreateArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID})
	if err != nil {
		t.Fatalf("runSnapshotCreate(terminal task) = %v, want nil", err)
	}
	if exec.createCalled {
		t.Errorf("agent Create attempted on a committed-terminal task; must abort without contacting the agent")
	}
	if st.manifestApplied {
		t.Errorf("manifest projected on a committed-terminal task; the abort path must not project")
	}
}

// TestRunSnapshotDelete_GCsOnlyOrphanedBlobs proves the fail-closed GC contract:
// the snapshot's manifest references two blobs ("aa" sole-referenced, "bb" still
// referenced by another snapshot). DereferenceSnapshotBlobs returns only the
// orphaned ["aa"]; the executor Delete must be handed ONLY ["aa"], never the
// shared "bb". Task finalized success.
func TestRunSnapshotDelete_GCsOnlyOrphanedBlobs(t *testing.T) {
	st := runningRuntimeStore()
	st.snapshot.DeletedAt = ptrNow()
	st.snapshot.Disks = []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
		{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
	}
	st.orphanedReturn = []string{"aa"} // bb stays referenced by another snapshot

	exec := &fakeSnapshotExecutor{}
	err := runSnapshotDelete(context.Background(), st, exec, discardLogger(),
		SnapshotDeleteArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID})
	if err != nil {
		t.Fatalf("runSnapshotDelete = %v, want nil", err)
	}

	if got, want := st.dereferenceDigests, []string{"aa", "bb"}; !sameStrings(got, want) {
		t.Errorf("DereferenceSnapshotBlobs digests = %v, want all manifest digests %v", got, want)
	}
	if !exec.deleteCalled {
		t.Fatalf("executor Delete not called")
	}
	if got, want := exec.deleteBlobs, []string{"aa"}; !sameStrings(got, want) {
		t.Errorf("agent Delete orphaned blobs = %v, want ONLY the orphaned %v (shared bb must never be GC'd)", got, want)
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusSuccess {
		t.Errorf("task finalized = (%v, %q), want (true, success)", st.finalized, st.finalizedStatus)
	}
}

func ptrNow() *time.Time { t := time.Now().UTC(); return &t }

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
