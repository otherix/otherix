// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// fakeDrainStore is a hand-written DrainWorkerStore double. It models a node
// plus a VM set that drains realistically: a VM enqueued for migration stays
// LISTED while it migrates (mirroring the real pinned-index list, which drops a
// VM only when its migration completes and re-pins to the target) and a second
// CreateMigration for a still-migrating VM returns store.ErrMigrationActiveExists,
// exactly like the real per-VM guard. After completeAfterPolls polls the VM
// completes and drops from the list, so the level-triggered saga converges.
type fakeDrainStore struct {
	mu sync.Mutex

	nodeID uuid.UUID

	// Node shape returned by NodeByID. By default a draining, owned node with a
	// recent heartbeat so drainActive passes and the staleness branch does not
	// fire. Tests override these to exercise the ownership / not-found / dead-node
	// branches.
	nodeStatus   store.NodeStatus
	drainTaskID  *uuid.UUID
	heartbeatAt  *time.Time
	nodeNotFound bool

	vms             []store.NodeVMRef
	targetAvailable bool

	// modelMigration, when set, makes the fake track per-VM migrating state: an
	// enqueued VM stays listed and migrates for completeAfterPolls polls, then
	// drops. While migrating, CreateMigration for it returns
	// store.ErrMigrationActiveExists.
	modelMigration     bool
	completeAfterPolls int
	migrating          map[uuid.UUID]int // VM id -> polls remaining until it drops

	// activeMigrationsForcesExists, when set, makes CreateMigration always return
	// store.ErrMigrationActiveExists (a VM that already has an active migration),
	// regardless of modelMigration.
	activeMigrationsForcesExists bool

	// activeBaseline is reported by ActiveSourceMigrationCount when modelMigration
	// is off; with modelMigration on, the live migrating count is reported instead.
	activeBaseline int

	// maxConcurrent overrides the DrainMaxConcurrentMigrations cap when non-zero.
	maxConcurrent int32

	// Recorded outcomes.
	migrateCalls         int
	updateTaskRunningHit bool
	finishDrainHit       bool
	finalizeTaskOnlyHit  bool
	finalStatus          store.TaskStatus
	finalNodeStatus      store.NodeStatus
	finalResult          []byte
}

func newFakeDrainStore(nodeID uuid.UUID) *fakeDrainStore {
	hb := time.Now()
	return &fakeDrainStore{
		nodeID:             nodeID,
		nodeStatus:         store.NodeStatusDraining,
		heartbeatAt:        &hb,
		completeAfterPolls: 1,
		migrating:          map[uuid.UUID]int{},
	}
}

func (f *fakeDrainStore) node() store.Node {
	return store.Node{
		ID:              f.nodeID,
		Status:          f.nodeStatus,
		DrainTaskID:     f.drainTaskID,
		LastHeartbeatAt: f.heartbeatAt,
	}
}

func (f *fakeDrainStore) NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodeNotFound {
		return store.Node{}, store.ErrNotFound
	}
	return f.node(), nil
}

func (f *fakeDrainStore) UpdateTaskRunning(ctx context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateTaskRunningHit = true
	return false, nil
}

func (f *fakeDrainStore) ListVMRefsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]store.NodeVMRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modelMigration {
		// A migrating VM stays listed until its migration completes (countdown
		// reaches zero), then drops - the real pinned-index re-pin to the target.
		kept := f.vms[:0:0]
		for _, v := range f.vms {
			left, migrating := f.migrating[v.ID]
			if migrating {
				if left <= 0 {
					delete(f.migrating, v.ID)
					continue // completed: re-pinned to target, no longer on this node
				}
				f.migrating[v.ID] = left - 1
			}
			kept = append(kept, v)
		}
		f.vms = kept
	}
	out := make([]store.NodeVMRef, len(f.vms))
	copy(out, f.vms)
	return out, nil
}

func (f *fakeDrainStore) ActiveSourceMigrationCount(ctx context.Context, nodeID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modelMigration {
		return len(f.migrating), nil
	}
	return f.activeBaseline, nil
}

func (f *fakeDrainStore) VMByID(ctx context.Context, id uuid.UUID) (store.VM, error) {
	return store.VM{ID: id, CpuCores: 1, MemoryMib: 512}, nil
}

func (f *fakeDrainStore) ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error) {
	return nil, nil
}

func (f *fakeDrainStore) ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error) {
	return []store.VMDisk{{ID: uuid.New(), VmID: vmID, StoragePoolID: uuid.New()}}, nil
}

func (f *fakeDrainStore) StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error) {
	return store.StoragePool{ID: id, Name: "default"}, nil
}

func (f *fakeDrainStore) CreateMigration(ctx context.Context, p store.CreateMigrationParams, args queue.JobArgs) (store.Migration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeMigrationsForcesExists {
		return store.Migration{}, store.ErrMigrationActiveExists
	}
	if f.modelMigration {
		// Per-VM guard: a VM already migrating cannot be re-enqueued.
		if _, ok := f.migrating[p.VmID]; ok {
			return store.Migration{}, store.ErrMigrationActiveExists
		}
		f.migrating[p.VmID] = f.completeAfterPolls
	}
	f.migrateCalls++
	return store.Migration{ID: p.ID}, nil
}

func (f *fakeDrainStore) FinishNodeDrain(ctx context.Context, nodeID, taskID uuid.UUID, status store.TaskStatus, result []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishDrainHit = true
	f.finalStatus = status
	f.finalNodeStatus = store.NodeStatusCordoned
	f.finalResult = result
	return nil
}

func (f *fakeDrainStore) UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizeTaskOnlyHit = true
	f.finalStatus = arg.Status
	f.finalResult = arg.Result
	return nil
}

func (f *fakeDrainStore) DrainCancelRequested(ctx context.Context, taskID uuid.UUID) (bool, error) {
	return false, nil
}

func (f *fakeDrainStore) DrainMaxConcurrentMigrations(ctx context.Context) (int32, error) {
	if f.maxConcurrent != 0 {
		return f.maxConcurrent, nil
	}
	return 5, nil
}

// fakePlacer returns a decision when a target is available, else an error
// (the saga reads "no eligible target" off the error).
type fakePlacer struct{ available bool }

func (p fakePlacer) Place(context.Context, scheduler.PlacementRequest) (scheduler.PlacementDecision, error) {
	if p.available {
		return scheduler.PlacementDecision{}, nil
	}
	return scheduler.PlacementDecision{}, errors.New("no eligible target")
}

// fixedClock never advances; advancingClock advances per call. Both satisfy the
// clock seam.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time {
	if c.t.IsZero() {
		return time.Unix(0, 0)
	}
	return c.t
}

type advancingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *advancingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.t.IsZero() {
		c.t = time.Now()
	}
	c.t = c.t.Add(time.Second)
	return c.t
}

func testDrainCfg() DrainConfig {
	return DrainConfig{PollInterval: time.Millisecond, DeadNodeGrace: time.Hour}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDrainConvergesToSuccess(t *testing.T) {
	nodeID := uuid.New()
	taskID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	fs.drainTaskID = &taskID
	fs.vms = []store.NodeVMRef{{ID: uuid.New(), Name: "vm-a"}, {ID: uuid.New(), Name: "vm-b"}}
	fs.targetAvailable = true
	fs.modelMigration = true // VMs stay listed while migrating, drop on completion

	err := runDrain(context.Background(), fs, fakePlacer{available: fs.targetAvailable}, fixedClock{}, testDrainCfg(), discardLogger(),
		NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: 1 << 40})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if fs.finalStatus != store.TaskStatusSuccess {
		t.Errorf("final status = %v, want success", fs.finalStatus)
	}
	if fs.finalNodeStatus != store.NodeStatusCordoned {
		t.Errorf("final node status = %v, want cordoned", fs.finalNodeStatus)
	}
	var res DrainResult
	_ = json.Unmarshal(fs.finalResult, &res)
	if res.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", res.Remaining)
	}
}

func TestDrainTimeoutLeavesVMsAndCordons(t *testing.T) {
	nodeID := uuid.New()
	taskID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	fs.drainTaskID = &taskID
	fs.vms = []store.NodeVMRef{{ID: uuid.New(), Name: "vm-stuck"}}
	fs.targetAvailable = false

	err := runDrain(context.Background(), fs, fakePlacer{available: fs.targetAvailable}, fixedClock{}, testDrainCfg(), discardLogger(),
		NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: 0})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if fs.finalStatus != store.TaskStatusFailed {
		t.Errorf("final status = %v, want failed", fs.finalStatus)
	}
	var res DrainResult
	_ = json.Unmarshal(fs.finalResult, &res)
	if res.Code != drainCodeTimeout || res.Remaining != 1 {
		t.Errorf("result = %+v, want timeout/remaining=1", res)
	}
	if fs.migrateCalls != 0 {
		t.Errorf("migrateCalls = %d, want 0 (no target, never enqueue)", fs.migrateCalls)
	}
}

func TestDrainNoTargetNeverEnqueues(t *testing.T) {
	nodeID := uuid.New()
	taskID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	fs.drainTaskID = &taskID
	fs.vms = []store.NodeVMRef{{ID: uuid.New(), Name: "vm-x"}}
	fs.targetAvailable = false

	// advancingClock advances one second per call, so the loop reaches the
	// deadline within a couple of polls and never enqueues for a target-less VM.
	_ = runDrain(context.Background(), fs, fakePlacer{available: fs.targetAvailable}, &advancingClock{}, testDrainCfg(), discardLogger(),
		NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: time.Now().Add(time.Second).Unix()})
	if fs.migrateCalls != 0 {
		t.Errorf("migrateCalls = %d, want 0", fs.migrateCalls)
	}
}

// TestDrainRedeliveryAfterFinalizeIsNoop proves the ownership guard runs BEFORE
// UpdateTaskRunning: a redelivered job whose node is no longer this drain's
// (cordoned / different DrainTaskID) must do nothing and never regress the
// finalized task back to running.
func TestDrainRedeliveryAfterFinalizeIsNoop(t *testing.T) {
	nodeID := uuid.New()
	taskID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	// Node already finalized back to cordoned, DrainTaskID cleared.
	fs.nodeStatus = store.NodeStatusCordoned
	fs.drainTaskID = nil
	fs.vms = []store.NodeVMRef{{ID: uuid.New(), Name: "vm-a"}}
	fs.targetAvailable = true

	err := runDrain(context.Background(), fs, fakePlacer{available: fs.targetAvailable}, fixedClock{}, testDrainCfg(), discardLogger(),
		NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: 1 << 40})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if fs.updateTaskRunningHit {
		t.Errorf("UpdateTaskRunning was called; the ownership guard must precede it")
	}
	if fs.finishDrainHit || fs.finalizeTaskOnlyHit {
		t.Errorf("finalized a superseded drain; want no-op (finish=%v finalizeOnly=%v)", fs.finishDrainHit, fs.finalizeTaskOnlyHit)
	}
}

// TestDrainNodeDeletedFinalizesTaskOnly proves a node force-deleted mid-drain
// (NodeByID -> ErrNotFound) finalizes the TASK only (cancelled), never calls
// FinishNodeDrain (no node row to flip), and returns nil.
func TestDrainNodeDeletedFinalizesTaskOnly(t *testing.T) {
	nodeID := uuid.New()
	taskID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	fs.nodeNotFound = true

	err := runDrain(context.Background(), fs, fakePlacer{available: true}, fixedClock{}, testDrainCfg(), discardLogger(),
		NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: 1 << 40})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if !fs.finalizeTaskOnlyHit {
		t.Errorf("UpdateTaskFinalized not called; want task-only finalize")
	}
	if fs.finishDrainHit {
		t.Errorf("FinishNodeDrain called for a deleted node; want task-only finalize")
	}
	if fs.finalStatus != store.TaskStatusCancelled {
		t.Errorf("final status = %v, want cancelled", fs.finalStatus)
	}
}

// TestDrainDeadNodeStaleHeartbeatFinalizes proves a draining, owned node whose
// last heartbeat is older than DeadNodeGrace is treated as dead: the saga
// finalizes failed{node_unreachable} instead of looping forever. The clock does
// not advance and the deadline is far in the future, so ONLY the staleness
// branch can terminate the loop - if it were missing the test would hang.
func TestDrainDeadNodeStaleHeartbeatFinalizes(t *testing.T) {
	nodeID := uuid.New()
	taskID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	fs.drainTaskID = &taskID
	fs.vms = []store.NodeVMRef{{ID: uuid.New(), Name: "vm-x"}}
	fs.targetAvailable = false // no target: VM never moves, loop relies on staleness
	stale := time.Unix(0, 0)
	fs.heartbeatAt = &stale

	cfg := testDrainCfg()
	cfg.DeadNodeGrace = time.Minute // heartbeat at epoch is far older

	err := runDrain(context.Background(), fs, fakePlacer{available: fs.targetAvailable}, fixedClock{t: time.Now()}, cfg, discardLogger(),
		NodeDrainRunArgs{TaskID: taskID, NodeID: nodeID, DeadlineUnix: 1 << 40})
	if err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if fs.finalStatus != store.TaskStatusFailed {
		t.Errorf("final status = %v, want failed", fs.finalStatus)
	}
	var res DrainResult
	_ = json.Unmarshal(fs.finalResult, &res)
	if res.Code != drainCodeNodeUnreachable {
		t.Errorf("code = %q, want %q", res.Code, drainCodeNodeUnreachable)
	}
}

// TestDrainSweepRespectsConcurrencyCap proves one sweep never enqueues more than
// (cap - baseline) new migrations: the cap counts migrations ALREADY in flight,
// so a sweep tops up to the cap and stops, regardless of how many VMs are listed.
func TestDrainSweepRespectsConcurrencyCap(t *testing.T) {
	nodeID := uuid.New()
	vms := []store.NodeVMRef{}
	for i := 0; i < 5; i++ {
		vms = append(vms, store.NodeVMRef{ID: uuid.New(), Name: "vm"})
	}

	cases := []struct {
		name        string
		maxConc     int32
		baseline    int
		wantEnqueue int
	}{
		{name: "headroom 2", maxConc: 2, baseline: 0, wantEnqueue: 2},
		{name: "partial headroom", maxConc: 2, baseline: 1, wantEnqueue: 1},
		{name: "at cap", maxConc: 2, baseline: 2, wantEnqueue: 0},
		{name: "over cap", maxConc: 2, baseline: 5, wantEnqueue: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeDrainStore(nodeID)
			fs.targetAvailable = true
			fs.maxConcurrent = tc.maxConc
			fs.activeBaseline = tc.baseline

			if err := drainSweep(context.Background(), fs, fakePlacer{available: true}, nodeID, vms, discardLogger()); err != nil {
				t.Fatalf("drainSweep: %v", err)
			}
			if fs.migrateCalls != tc.wantEnqueue {
				t.Errorf("migrateCalls = %d, want %d (cap=%d baseline=%d)", fs.migrateCalls, tc.wantEnqueue, tc.maxConc, tc.baseline)
			}
		})
	}
}

// TestDrainSkipsVMWithActiveMigration proves a VM whose CreateMigration reports
// store.ErrMigrationActiveExists is treated as enqueued=false with no error: the
// active counter is not bumped and no failure surfaces, so a redelivered saga
// cannot double-enqueue an already-migrating VM.
func TestDrainSkipsVMWithActiveMigration(t *testing.T) {
	nodeID := uuid.New()
	fs := newFakeDrainStore(nodeID)
	fs.targetAvailable = true
	fs.activeMigrationsForcesExists = true

	v := store.NodeVMRef{ID: uuid.New(), Name: "vm-busy"}
	enqueued, err := tryEvacuate(context.Background(), fs, fakePlacer{available: true}, nodeID, v, discardLogger())
	if err != nil {
		t.Fatalf("tryEvacuate: %v", err)
	}
	if enqueued {
		t.Errorf("enqueued = true, want false (VM already migrating)")
	}
	if fs.migrateCalls != 0 {
		t.Errorf("migrateCalls = %d, want 0 (active-exists is not a successful enqueue)", fs.migrateCalls)
	}
}
