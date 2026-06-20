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

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// snapWorkerStoreStub is the WorkerStore double for the snapshot worker tests. It
// records the calls the seam tests assert on (the manifest projection inputs, the
// finalize status, the dereference request) and drives the redelivery branch via
// taskTerminal. Unset reads return benign zero rows so a test only needs to set
// the fields its assertion touches.
type snapWorkerStoreStub struct {
	taskTerminal bool

	// task is the row TaskByID returns (the create worker reads its AgentTaskID
	// to decide POST-vs-resume). The zero value (nil AgentTaskID) is the first-run
	// case; a test sets task.AgentTaskID to drive the redelivery-resume branch.
	task store.Task

	snapshot  store.Snapshot
	vm        store.VM
	vmByIDErr error
	runtime   store.VMRuntime
	node      store.Node

	// placement map the delete path reads to resolve holder nodes WITHOUT a VM
	// lookup (digest -> holder node ids).
	placements map[string][]uuid.UUID

	// NodeByID resolution: a per-id error (nodeErrByID) wins, else a per-id row
	// (nodesByID), else the shared s.node. Lets a multi-holder test give one node a
	// gone (ErrNotFound) or transient error while another resolves normally.
	nodesByID   map[uuid.UUID]store.Node
	nodeErrByID map[uuid.UUID]error

	// recorded create-path calls
	manifestDisks   []store.SnapshotDisk
	manifestVMState store.VMStateAtSnapshot
	manifestNodeID  uuid.UUID
	manifestApplied bool

	// recorded agent_task_id persistence (the resume seam): the id stamped on the
	// task after the first PostSnapshot, before polling.
	persistedAgentTaskID *uuid.UUID
	agentTaskIDPersisted bool
	agentTaskIDCleared   bool

	// recorded delete-path calls
	dereferenceDigests []string
	orphanedReturn     []string

	// recorded terminal task outcome
	finalizedStatus store.TaskStatus
	finalized       bool

	// recorded snapshot-row error surfacing (the create-fail nicety): the id and
	// message handed to MarkSnapshotError, and whether it was called at all.
	markedErrorID  uuid.UUID
	markedErrorMsg string
	markedError    bool
}

func (s *snapWorkerStoreStub) UpdateTaskRunning(context.Context, uuid.UUID) (bool, error) {
	return s.taskTerminal, nil
}

func (s *snapWorkerStoreStub) TaskByID(context.Context, uuid.UUID) (store.Task, error) {
	return s.task, nil
}

func (s *snapWorkerStoreStub) UpdateTaskAgentTaskID(_ context.Context, arg store.UpdateTaskAgentTaskIDParams) error {
	s.agentTaskIDPersisted = true
	s.persistedAgentTaskID = arg.AgentTaskID
	return nil
}

func (s *snapWorkerStoreStub) ClearTaskAgentTaskID(context.Context, uuid.UUID) error {
	s.agentTaskIDCleared = true
	return nil
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
	return s.vm, s.vmByIDErr
}

func (s *snapWorkerStoreStub) VMRuntimeByID(context.Context, uuid.UUID) (store.VMRuntime, error) {
	return s.runtime, nil
}

func (s *snapWorkerStoreStub) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	if err, ok := s.nodeErrByID[id]; ok {
		return store.Node{}, err
	}
	if n, ok := s.nodesByID[id]; ok {
		return n, nil
	}
	return s.node, nil
}

func (s *snapWorkerStoreStub) SnapshotManifestApplied(_ context.Context, _, nodeID uuid.UUID, disks []store.SnapshotDisk, vmState store.VMStateAtSnapshot) error {
	s.manifestApplied = true
	s.manifestDisks = disks
	s.manifestVMState = vmState
	s.manifestNodeID = nodeID
	return nil
}

func (s *snapWorkerStoreStub) DereferenceSnapshotBlobs(_ context.Context, _ uuid.UUID, digests []string) ([]string, error) {
	s.dereferenceDigests = digests
	return s.orphanedReturn, nil
}

func (s *snapWorkerStoreStub) BlobPlacements(_ context.Context, digest string) ([]uuid.UUID, error) {
	return s.placements[digest], nil
}

func (s *snapWorkerStoreStub) MarkSnapshotError(_ context.Context, id uuid.UUID, msg string) error {
	s.markedError = true
	s.markedErrorID = id
	s.markedErrorMsg = msg
	return nil
}

// fakeSnapshotExecutor is the SnapshotExecutor double. Create is split into Post
// (the one-shot agent POST that returns the agent task id) and Poll (resumable
// against a persisted id), mirroring the migrations agent_task_id resume seam.
// postCalled records whether a NEW agent POST was issued - the resume test asserts
// it stays false on a redelivery that already carries an agent task id; pollTaskID
// records which id Poll ran against.
type fakeSnapshotExecutor struct {
	postCalled    bool
	postAgentTask uuid.UUID
	postErr       error

	pollCalled bool
	pollTaskID uuid.UUID
	pollResult CreateExecResult
	pollErr    error

	deleteCalled    bool
	deleteBlobs     []string
	deleteVMID      uuid.UUID
	deleteEndpoint  string
	deleteCallCount int
	deleteErr       error
}

func (e *fakeSnapshotExecutor) Post(context.Context, CreateExecArgs) (uuid.UUID, error) {
	e.postCalled = true
	return e.postAgentTask, e.postErr
}

func (e *fakeSnapshotExecutor) Poll(_ context.Context, endpoint string, agentTaskID uuid.UUID) (CreateExecResult, error) {
	e.pollCalled = true
	e.pollTaskID = agentTaskID
	return e.pollResult, e.pollErr
}

func (e *fakeSnapshotExecutor) Delete(_ context.Context, a DeleteExecArgs) error {
	e.deleteCalled = true
	e.deleteCallCount++
	e.deleteBlobs = a.OrphanedBlobs
	e.deleteVMID = a.VMID
	e.deleteEndpoint = a.AdvertisedEndpoint
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
	agentTaskID := uuid.New()
	exec := &fakeSnapshotExecutor{
		postAgentTask: agentTaskID,
		pollResult: CreateExecResult{
			Disks: []store.SnapshotDisk{
				{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
				{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
			},
			VMStateAtSnapshot: store.VmStateAtSnapshotRunning,
		},
	}

	taskID := uuid.New()
	st.task = store.Task{ID: taskID} // first run: no AgentTaskID yet
	raw, _ := json.Marshal(SnapshotCreateArgs{TaskID: taskID, SnapshotID: st.snapshot.ID})
	if err := CreateHandler(st, exec, discardLogger())(context.Background(), raw); err != nil {
		t.Fatalf("CreateHandler run = %v, want nil", err)
	}

	if !exec.postCalled {
		t.Errorf("executor Post not called on a first-run non-terminal task")
	}
	// The agent task id is persisted BEFORE polling so a crash mid-poll resumes
	// rather than re-POSTs (the resume seam).
	if !st.agentTaskIDPersisted {
		t.Errorf("UpdateTaskAgentTaskID not called after the first PostSnapshot; a crash mid-poll would re-POST")
	}
	if st.persistedAgentTaskID == nil || *st.persistedAgentTaskID != agentTaskID {
		t.Errorf("persisted agent_task_id = %v, want %v", st.persistedAgentTaskID, agentTaskID)
	}
	if !exec.pollCalled || exec.pollTaskID != agentTaskID {
		t.Errorf("Poll called=%v on id %v, want true on %v", exec.pollCalled, exec.pollTaskID, agentTaskID)
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

// TestRunSnapshotCreate_ResumesPollNotRepost_OnRedeliveryWithAgentTaskID is the
// seam-B regression: a task that ALREADY carries an agent_task_id (a redelivery
// while the agent capture is still running - worker crash mid-poll, or a
// finalize-failed retry) must RESUME polling the persisted id, NOT re-POST a
// second agent capture. A re-POST would trigger a second full-disk blockdev-backup
// whose blobs (if the disk changed) overwrite the manifest and orphan the first
// attempt's blob refs. Assert: zero new PostSnapshot, Poll resumes on the
// persisted agent task id, manifest projected, task finalized success.
func TestRunSnapshotCreate_ResumesPollNotRepost_OnRedeliveryWithAgentTaskID(t *testing.T) {
	st := runningRuntimeStore()
	resumedID := uuid.New()
	taskID := uuid.New()
	st.task = store.Task{ID: taskID, AgentTaskID: &resumedID} // redelivery: id already persisted

	exec := &fakeSnapshotExecutor{
		pollResult: CreateExecResult{
			Disks:             []store.SnapshotDisk{{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"}},
			VMStateAtSnapshot: store.VmStateAtSnapshotRunning,
		},
	}

	raw, _ := json.Marshal(SnapshotCreateArgs{TaskID: taskID, SnapshotID: st.snapshot.ID})
	if err := CreateHandler(st, exec, discardLogger())(context.Background(), raw); err != nil {
		t.Fatalf("CreateHandler run = %v, want nil", err)
	}

	if exec.postCalled {
		t.Errorf("PostSnapshot issued on a redelivery that already carries an agent_task_id; must resume, not re-POST a second capture")
	}
	if !exec.pollCalled || exec.pollTaskID != resumedID {
		t.Errorf("Poll resumed on id %v (called=%v), want resume on the persisted %v", exec.pollTaskID, exec.pollCalled, resumedID)
	}
	if !st.manifestApplied {
		t.Errorf("SnapshotManifestApplied not called on resumed-poll success")
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
	taskID := uuid.New()
	st.task = store.Task{ID: taskID}
	exec := &fakeSnapshotExecutor{postErr: cause}

	err := runSnapshotCreate(context.Background(), st, exec, discardLogger(),
		SnapshotCreateArgs{TaskID: taskID, SnapshotID: st.snapshot.ID})
	if !errors.Is(err, cause) {
		t.Errorf("runSnapshotCreate err = %v, want the executor cause", err)
	}
	if st.manifestApplied {
		t.Errorf("SnapshotManifestApplied called despite executor error; must not project on failure")
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusFailed {
		t.Errorf("task finalized = (%v, %q), want (true, failed)", st.finalized, st.finalizedStatus)
	}
	// The create-fail path must ALSO surface the failure onto the snapshot row, so
	// an operator listing snapshots sees error (not a permanently-creating row).
	if !st.markedError {
		t.Errorf("snapshot row not flipped to error on create failure; MarkSnapshotError must be called")
	}
	if st.markedErrorID != st.snapshot.ID {
		t.Errorf("MarkSnapshotError id = %v, want the snapshot id %v", st.markedErrorID, st.snapshot.ID)
	}
	if st.markedErrorMsg == "" {
		t.Errorf("MarkSnapshotError msg = empty, want the failure message")
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
	if exec.postCalled || exec.pollCalled {
		t.Errorf("agent contacted on a committed-terminal task; must abort without POST or Poll")
	}
	if st.manifestApplied {
		t.Errorf("manifest projected on a committed-terminal task; the abort path must not project")
	}
}

// TestRunSnapshotCreate_AgentTask404_ClearsIDForRePost is the agent-restart resume
// regression: a redelivery resumes Poll on the persisted agent_task_id, but the
// agent restarted and lost that task, so Poll returns a permanent 404. The worker
// must CLEAR the persisted agent_task_id (so the NEXT redelivery re-POSTs and the
// agent's (vm,name) idempotency reconciles) before failing the task - not leave the
// stale id to be polled to a hard failure forever.
func TestRunSnapshotCreate_AgentTask404_ClearsIDForRePost(t *testing.T) {
	st := runningRuntimeStore()
	resumedID := uuid.New()
	taskID := uuid.New()
	st.task = store.Task{ID: taskID, AgentTaskID: &resumedID} // redelivery: resume path
	exec := &fakeSnapshotExecutor{pollErr: &agentclient.AgentError{Status: 404, Code: "not_found"}}

	err := runSnapshotCreate(context.Background(), st, exec, discardLogger(),
		SnapshotCreateArgs{TaskID: taskID, SnapshotID: st.snapshot.ID})
	if err == nil {
		t.Fatalf("runSnapshotCreate = nil, want the poll-404 error")
	}
	if !st.agentTaskIDCleared {
		t.Errorf("agent_task_id NOT cleared on a permanent 404; the redelivery would re-poll the vanished task forever instead of re-POSTing")
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusFailed {
		t.Errorf("task finalized = (%v, %q), want (true, failed)", st.finalized, st.finalizedStatus)
	}
}

// TestRunSnapshotDelete_GCsOnlyOrphanedBlobs proves the fail-closed GC contract:
// the snapshot's manifest references two blobs ("aa" sole-referenced, "bb" still
// referenced by another snapshot). DereferenceSnapshotBlobs returns only the
// orphaned ["aa"]; the executor Delete must be handed ONLY ["aa"], never the
// shared "bb". The holder node is resolved through the PLACEMENT map (not the VM),
// the agent Delete is keyed on the snapshot's vm_id, the orphaned placement entry
// is dropped, and the task is finalized success.
func TestRunSnapshotDelete_GCsOnlyOrphanedBlobs(t *testing.T) {
	st := runningRuntimeStore()
	st.snapshot.DeletedAt = ptrNow()
	st.snapshot.Disks = []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
		{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
	}
	st.orphanedReturn = []string{"aa"} // bb stays referenced by another snapshot
	st.placements = map[string][]uuid.UUID{"aa": {st.node.ID}, "bb": {st.node.ID}}

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
	if exec.deleteVMID != st.snapshot.VmID {
		t.Errorf("agent Delete vm_id = %v, want the snapshot's vm_id %v", exec.deleteVMID, st.snapshot.VmID)
	}
	if exec.deleteEndpoint != st.node.AdvertisedEndpoint {
		t.Errorf("agent Delete endpoint = %q, want the placement holder's %q", exec.deleteEndpoint, st.node.AdvertisedEndpoint)
	}
	if got, want := exec.deleteBlobs, []string{"aa"}; !sameStrings(got, want) {
		t.Errorf("agent Delete orphaned blobs = %v, want ONLY the orphaned %v (shared bb must never be GC'd)", got, want)
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusSuccess {
		t.Errorf("task finalized = (%v, %q), want (true, success)", st.finalized, st.finalizedStatus)
	}
}

// TestRunSnapshotDelete_VMDeleted_StillDeletesViaPlacement is the core regression
// for the standalone-snapshot promise: the source VM is GONE (VMByID errors), yet
// the delete must still locate the blob's holder node through the placement map and
// drive the agent GC. A delete path that resolves the node via the VM (the bug)
// fails here; the placement path succeeds.
func TestRunSnapshotDelete_VMDeleted_StillDeletesViaPlacement(t *testing.T) {
	st := runningRuntimeStore()
	st.vmByIDErr = errors.New("store: not found") // VM already deleted
	st.snapshot.DeletedAt = ptrNow()
	st.snapshot.Disks = []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
	}
	st.orphanedReturn = []string{"aa"}
	st.placements = map[string][]uuid.UUID{"aa": {st.node.ID}}

	exec := &fakeSnapshotExecutor{}
	if err := runSnapshotDelete(context.Background(), st, exec, discardLogger(),
		SnapshotDeleteArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID}); err != nil {
		t.Fatalf("runSnapshotDelete with deleted VM = %v, want nil (placement path is VM-independent)", err)
	}
	if !exec.deleteCalled || exec.deleteVMID != st.snapshot.VmID {
		t.Errorf("agent Delete called=%v vm_id=%v, want true on the snapshot vm_id %v", exec.deleteCalled, exec.deleteVMID, st.snapshot.VmID)
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusSuccess {
		t.Errorf("task finalized = (%v, %q), want (true, success)", st.finalized, st.finalizedStatus)
	}
}

// TestRunSnapshotDelete_HolderNodeGone_SkipsAndContinues pins the fail-toward-
// inaction skip: of two holder nodes, one's row is GONE (NodeByID -> ErrNotFound,
// a permanently decommissioned node). The worker must SKIP it (its blob is
// unreachable - a recoverable leak), still drive the agent delete on the reachable
// holder, and finalize success. It must NOT wedge or fail the task.
func TestRunSnapshotDelete_HolderNodeGone_SkipsAndContinues(t *testing.T) {
	st := runningRuntimeStore()
	st.snapshot.DeletedAt = ptrNow()
	st.snapshot.Disks = []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
		{Index: 1, Device: "virtio1", SHA256: "bb", SizeBytes: 20, Format: "qcow2"},
	}
	st.orphanedReturn = []string{"aa", "bb"}
	liveNode := store.Node{ID: uuid.New(), AdvertisedEndpoint: "https://live:9443"}
	goneNode := uuid.New()
	st.nodesByID = map[uuid.UUID]store.Node{liveNode.ID: liveNode}
	st.nodeErrByID = map[uuid.UUID]error{goneNode: store.ErrNotFound}
	st.placements = map[string][]uuid.UUID{"aa": {liveNode.ID}, "bb": {goneNode}}

	exec := &fakeSnapshotExecutor{}
	if err := runSnapshotDelete(context.Background(), st, exec, discardLogger(),
		SnapshotDeleteArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID}); err != nil {
		t.Fatalf("runSnapshotDelete = %v, want nil (gone holder is skipped, not fatal)", err)
	}
	if exec.deleteCallCount != 1 {
		t.Errorf("agent Delete call count = %d, want 1 (only the reachable holder; the gone node is skipped)", exec.deleteCallCount)
	}
	if exec.deleteEndpoint != liveNode.AdvertisedEndpoint {
		t.Errorf("agent Delete endpoint = %q, want the reachable holder %q", exec.deleteEndpoint, liveNode.AdvertisedEndpoint)
	}
	if got, want := exec.deleteBlobs, []string{"aa"}; !sameStrings(got, want) {
		t.Errorf("reachable holder orphans = %v, want only its own digest %v", got, want)
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusSuccess {
		t.Errorf("task finalized = (%v, %q), want (true, success)", st.finalized, st.finalizedStatus)
	}
}

// TestRunSnapshotDelete_HolderNodeTransientError_FailsTask pins the retry path: a
// holder whose NodeByID errors with a NON-ErrNotFound (transient) cause must FAIL
// the task so the dispatcher retries - not be silently skipped (that would leak a
// blob on a node that is merely temporarily unresolvable).
func TestRunSnapshotDelete_HolderNodeTransientError_FailsTask(t *testing.T) {
	st := runningRuntimeStore()
	st.snapshot.DeletedAt = ptrNow()
	st.snapshot.Disks = []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
	}
	st.orphanedReturn = []string{"aa"}
	flaky := uuid.New()
	st.nodeErrByID = map[uuid.UUID]error{flaky: errors.New("etcd timeout")}
	st.placements = map[string][]uuid.UUID{"aa": {flaky}}

	exec := &fakeSnapshotExecutor{}
	err := runSnapshotDelete(context.Background(), st, exec, discardLogger(),
		SnapshotDeleteArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID})
	if err == nil {
		t.Fatalf("runSnapshotDelete = nil, want the node-resolution error (task must fail and retry)")
	}
	if exec.deleteCalled {
		t.Errorf("agent Delete called despite an unresolved holder node; want no agent contact")
	}
	if !st.finalized || st.finalizedStatus != store.TaskStatusFailed {
		t.Errorf("task finalized = (%v, %q), want (true, failed)", st.finalized, st.finalizedStatus)
	}
	// The DELETE path must never flip the (already soft-deleted) row to error - that
	// surfacing is scoped to the CREATE kind only.
	if st.markedError {
		t.Errorf("MarkSnapshotError called on the delete path; must be create-kind only")
	}
}

// TestRunSnapshotDelete_NoPlacement_FailsOpen pins the fail-toward-inaction branch:
// a snapshot with NO placement entries (a pre-placement-map row, or a zero-disk
// row) cannot have its holder resolved VM-independently. The worker must NOT wedge
// (or contact any agent) - it finalizes success, leaking any blob for a later
// sweep, because wedging the task forever is worse than a recoverable leak.
func TestRunSnapshotDelete_NoPlacement_FailsOpen(t *testing.T) {
	st := runningRuntimeStore()
	st.snapshot.DeletedAt = ptrNow()
	st.snapshot.Disks = []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "aa", SizeBytes: 10, Format: "qcow2"},
	}
	st.orphanedReturn = []string{"aa"}
	st.placements = nil // no placement seed

	exec := &fakeSnapshotExecutor{}
	if err := runSnapshotDelete(context.Background(), st, exec, discardLogger(),
		SnapshotDeleteArgs{TaskID: uuid.New(), SnapshotID: st.snapshot.ID}); err != nil {
		t.Fatalf("runSnapshotDelete = %v, want nil (fail-open on missing placement)", err)
	}
	if exec.deleteCalled {
		t.Errorf("agent Delete called with no resolvable holder; want no agent contact")
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
