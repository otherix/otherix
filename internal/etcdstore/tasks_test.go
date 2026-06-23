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

	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// With EnqueueTask / CancelPendingTask implemented, the etcd store satisfies the
// tasks and storage-pools handler contracts (the queue-bound producers). Only
// vms.Store remains (CreateScheduledVM + placement).
var (
	_ taskshandlers.Store        = (*etcdstore.Store)(nil)
	_ storagepoolshandlers.Store = (*etcdstore.Store)(nil)
)

// testJobArgs is a minimal queue.JobArgs payload for the enqueue tests.
type testJobArgs struct {
	Foo string `json:"foo"`
}

func (testJobArgs) Kind() string { return "test.job" }

func taskParams(status store.TaskStatus, creator *uuid.UUID) store.CreateTaskParams {
	return store.CreateTaskParams{
		ID: uuid.New(), Type: "vm.create", Status: status, ResourceType: "vm",
		Args: []byte(`{}`), MaxAttempts: 3, CreatedBy: creator,
	}
}

// TestActiveVMDeleteTaskVMIDs asserts the projection lookup returns exactly the
// VMs with a non-terminal vm.delete task: pending and running are in the set,
// a terminal (success) delete and a different task type are excluded.
func TestActiveVMDeleteTaskVMIDs(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	vmPending := uuid.New()  // pending vm.delete -> in set
	vmRunning := uuid.New()  // running vm.delete -> in set
	vmDone := uuid.New()     // success vm.delete -> excluded (terminal)
	vmCreating := uuid.New() // pending vm.create -> excluded (wrong type)

	enqueue := func(typ string, res uuid.UUID) uuid.UUID {
		t.Helper()
		p := store.CreateTaskParams{
			ID: uuid.New(), Type: typ, Status: store.TaskStatusPending, ResourceType: "vm",
			ResourceID: &res, Args: []byte(`{}`), MaxAttempts: 3,
		}
		if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
			t.Fatalf("EnqueueTask(%s): %v", typ, err)
		}
		return p.ID
	}

	enqueue("vm.delete", vmPending)
	runID := enqueue("vm.delete", vmRunning)
	if _, err := s.UpdateTaskRunning(ctx, runID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	doneID := enqueue("vm.delete", vmDone)
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: doneID, Status: store.TaskStatusSuccess}); err != nil {
		t.Fatalf("UpdateTaskFinalized: %v", err)
	}
	enqueue("vm.create", vmCreating)

	got, err := s.ActiveVMDeleteTaskVMIDs(ctx)
	if err != nil {
		t.Fatalf("ActiveVMDeleteTaskVMIDs: %v", err)
	}
	for _, want := range []uuid.UUID{vmPending, vmRunning} {
		if _, ok := got[want]; !ok {
			t.Errorf("vm %v with active vm.delete missing from set", want)
		}
	}
	for _, excl := range []uuid.UUID{vmDone, vmCreating} {
		if _, ok := got[excl]; ok {
			t.Errorf("vm %v should not be in the active-delete set", excl)
		}
	}
	if len(got) != 2 {
		t.Errorf("set size = %d, want 2", len(got))
	}
}

// vmCreateJobArgs is a minimal queue.JobArgs payload that carries a
// source_snapshot_id, matching the wire shape of the production VMCreateArgs the
// vm.create enqueue path persists. ActiveCreatesReferencingSnapshot decodes this
// off the task's Args JSON.
type vmCreateJobArgs struct {
	SourceSnapshotID *uuid.UUID `json:"source_snapshot_id,omitempty"`
}

func (vmCreateJobArgs) Kind() string { return "vm.create" }

// TestActiveCreatesReferencingSnapshot asserts the guard counts exactly the
// pending|running vm.create tasks whose source_snapshot_id is the queried
// snapshot: a pending and a running create are counted, while a terminal
// (success) create, an image-sourced create (no source_snapshot_id), a create
// referencing a different snapshot, and a non-create task are all excluded. The
// count drops to zero once the matching creates reach a terminal state.
func TestActiveCreatesReferencingSnapshot(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	snapID := uuid.New()
	otherSnapID := uuid.New()

	enqueueCreate := func(src *uuid.UUID, status store.TaskStatus) uuid.UUID {
		t.Helper()
		argsJSON := []byte(`{}`)
		if src != nil {
			argsJSON = []byte(`{"source_snapshot_id":"` + src.String() + `"}`)
		}
		res := uuid.New()
		p := store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.create", Status: store.TaskStatusPending,
			ResourceType: "vm", ResourceID: &res, Args: argsJSON, MaxAttempts: 3,
		}
		if _, err := s.EnqueueTask(ctx, p, vmCreateJobArgs{SourceSnapshotID: src}); err != nil {
			t.Fatalf("EnqueueTask: %v", err)
		}
		switch status {
		case store.TaskStatusRunning:
			if _, err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
				t.Fatalf("UpdateTaskRunning: %v", err)
			}
		case store.TaskStatusSuccess, store.TaskStatusFailed, store.TaskStatusCancelled:
			if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: p.ID, Status: status}); err != nil {
				t.Fatalf("UpdateTaskFinalized: %v", err)
			}
		}
		return p.ID
	}

	enqueueCreate(&snapID, store.TaskStatusPending)      // counted
	enqueueCreate(&snapID, store.TaskStatusRunning)      // counted
	enqueueCreate(&snapID, store.TaskStatusSuccess)      // terminal -> excluded
	enqueueCreate(nil, store.TaskStatusRunning)          // image-sourced -> excluded
	enqueueCreate(&otherSnapID, store.TaskStatusRunning) // different snapshot -> excluded

	// A non-create task referencing the snapshot id is not a vm.create -> excluded.
	res := uuid.New()
	if _, err := s.EnqueueTask(ctx, store.CreateTaskParams{
		ID: uuid.New(), Type: "vm.delete", Status: store.TaskStatusPending, ResourceType: "vm",
		ResourceID: &res, Args: []byte(`{"source_snapshot_id":"` + snapID.String() + `"}`), MaxAttempts: 3,
	}, vmCreateJobArgs{SourceSnapshotID: &snapID}); err != nil {
		t.Fatalf("EnqueueTask(vm.delete): %v", err)
	}

	got, err := s.ActiveCreatesReferencingSnapshot(ctx, snapID)
	if err != nil {
		t.Fatalf("ActiveCreatesReferencingSnapshot: %v", err)
	}
	if got != 2 {
		t.Errorf("ActiveCreatesReferencingSnapshot(%v) = %d, want 2 (pending + running)", snapID, got)
	}

	gotOther, err := s.ActiveCreatesReferencingSnapshot(ctx, otherSnapID)
	if err != nil {
		t.Fatalf("ActiveCreatesReferencingSnapshot(other): %v", err)
	}
	if gotOther != 1 {
		t.Errorf("ActiveCreatesReferencingSnapshot(%v) = %d, want 1", otherSnapID, gotOther)
	}

	absent := uuid.New()
	gotAbsent, err := s.ActiveCreatesReferencingSnapshot(ctx, absent)
	if err != nil {
		t.Fatalf("ActiveCreatesReferencingSnapshot(absent): %v", err)
	}
	if gotAbsent != 0 {
		t.Errorf("ActiveCreatesReferencingSnapshot(%v) = %d, want 0", absent, gotAbsent)
	}
}

func TestEnqueueTaskWritesTaskAndJob(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	id, err := s.EnqueueTask(ctx, p, testJobArgs{Foo: "bar"})
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	if id != p.ID {
		t.Errorf("EnqueueTask id = %v, want %v", id, p.ID)
	}
	got, err := s.TaskByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if got.Status != store.TaskStatusPending || got.JobID == nil || *got.JobID < 1 {
		t.Errorf("task = %+v, want pending + job ref stamped", got)
	}
}

func TestEnqueueTaskJobSequenceMonotonic(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	var prev int64
	for i := 0; i < 3; i++ {
		p := taskParams(store.TaskStatusPending, nil)
		if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
			t.Fatalf("EnqueueTask %d: %v", i, err)
		}
		got, _ := s.TaskByID(ctx, p.ID)
		if *got.JobID <= prev {
			t.Errorf("job seq not monotonic: %d after %d", *got.JobID, prev)
		}
		prev = *got.JobID
	}
}

func TestCancelPendingTask(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	cancelled, err := s.CancelPendingTask(ctx, p.ID, got.JobID)
	if err != nil {
		t.Fatalf("CancelPendingTask: %v", err)
	}
	if cancelled.Status != store.TaskStatusCancelled || cancelled.FinishedAt == nil {
		t.Errorf("cancelled = %+v, want cancelled + finished_at", cancelled)
	}
	// Second cancel: no longer pending.
	if _, err := s.CancelPendingTask(ctx, p.ID, got.JobID); !errors.Is(err, store.ErrTaskNotCancellable) {
		t.Errorf("re-cancel = %v, want store.ErrTaskNotCancellable", err)
	}
	// Missing task.
	if _, err := s.CancelPendingTask(ctx, uuid.New(), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cancel missing = %v, want store.ErrNotFound", err)
	}
}

func TestListTasksScopeAndFilters(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	alice := uuid.New()
	bob := uuid.New()
	// Alice: one pending, one (seeded) success; Bob: one pending.
	pa := taskParams(store.TaskStatusPending, &alice)
	if _, err := s.EnqueueTask(ctx, pa, testJobArgs{}); err != nil {
		t.Fatalf("enqueue alice: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	pb := taskParams(store.TaskStatusPending, &bob)
	if _, err := s.EnqueueTask(ctx, pb, testJobArgs{}); err != nil {
		t.Fatalf("enqueue bob: %v", err)
	}

	// Any-scope sees both, newest first.
	all, err := s.ListTasksAny(ctx, store.ListTasksAnyParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	if len(all) != 2 || all[0].ID != pb.ID {
		t.Errorf("ListTasksAny = %v, want bob first", taskIDs(all))
	}

	// Own-scope: only Alice's.
	own, err := s.ListTasksOwn(ctx, store.ListTasksOwnParams{CreatedBy: &alice, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTasksOwn: %v", err)
	}
	if len(own) != 1 || own[0].ID != pa.ID {
		t.Errorf("ListTasksOwn(alice) = %v, want [%v]", taskIDs(own), pa.ID)
	}

	// Status filter.
	pending := "pending"
	filtered, err := s.ListTasksAny(ctx, store.ListTasksAnyParams{StatusFilter: &pending, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTasksAny(status): %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("pending filter = %d, want 2", len(filtered))
	}
	done := "success"
	noneDone, err := s.ListTasksAny(ctx, store.ListTasksAnyParams{StatusFilter: &done, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTasksAny(success): %v", err)
	}
	if len(noneDone) != 0 {
		t.Errorf("success filter = %d, want 0", len(noneDone))
	}
}

func taskIDs(ts []store.Task) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestUpdateTaskRunningStampsAndIncrements(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	if _, err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	if got.Status != store.TaskStatusRunning || got.Attempts != 1 || got.StartedAt == nil {
		t.Errorf("after first run: status=%v attempts=%d started_at=%v, want running/1/non-nil",
			got.Status, got.Attempts, got.StartedAt)
	}
	firstStarted := *got.StartedAt

	// Second transition: attempts++ but started_at is coalesced (unchanged).
	if _, err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
		t.Fatalf("UpdateTaskRunning (retry): %v", err)
	}
	got, _ = s.TaskByID(ctx, p.ID)
	if got.Attempts != 2 || got.StartedAt == nil || !got.StartedAt.Equal(firstStarted) {
		t.Errorf("after retry: attempts=%d started_at=%v, want 2 and unchanged %v",
			got.Attempts, got.StartedAt, firstStarted)
	}

	// Missing task.
	if _, err := s.UpdateTaskRunning(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskRunning(missing) = %v, want store.ErrNotFound", err)
	}
}

func TestUpdateTaskRunningSkipsCommittedTerminalButPromotesFailed(t *testing.T) {
	// Committed-terminal statuses (success / cancelled) prove a non-idempotent
	// projection already committed: UpdateTaskRunning must leave them untouched.
	// failed is retryable (failRun finalizes failed but the dispatcher requeues
	// the job), so a failed task must still transition to running and bump
	// Attempts on redelivery.
	for _, tc := range []struct {
		name    string
		status  store.TaskStatus
		promote bool // does a redelivery transition it to running + bump attempts?
	}{
		{"success", store.TaskStatusSuccess, false},
		{"cancelled", store.TaskStatusCancelled, false},
		{"failed", store.TaskStatusFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := startStore(t)
			ctx := context.Background()
			p := taskParams(store.TaskStatusPending, nil)
			if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
				t.Fatalf("EnqueueTask: %v", err)
			}

			// Drive the task to its terminal state (one delivery + finalize).
			if _, err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
				t.Fatalf("UpdateTaskRunning: %v", err)
			}
			if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
				ID:     p.ID,
				Status: tc.status,
			}); err != nil {
				t.Fatalf("UpdateTaskFinalized: %v", err)
			}
			before, _ := s.TaskByID(ctx, p.ID)

			// A worker redelivery calls UpdateTaskRunning again at the top of the
			// delivery.
			terminal, err := s.UpdateTaskRunning(ctx, p.ID)
			if err != nil {
				t.Fatalf("UpdateTaskRunning (redelivery) = %v, want nil", err)
			}
			// A committed-terminal task signals alreadyTerminal=true; a retryable
			// (failed) task signals false (it promotes to running).
			if terminal == tc.promote {
				t.Errorf("alreadyTerminal after %s redelivery = %v, want %v", tc.name, terminal, !tc.promote)
			}
			after, _ := s.TaskByID(ctx, p.ID)

			if tc.promote {
				if after.Status != store.TaskStatusRunning {
					t.Errorf("status after %s redelivery = %v, want running (retryable)", tc.name, after.Status)
				}
				if after.Attempts != before.Attempts+1 {
					t.Errorf("attempts after %s redelivery = %d, want %d (bumped)", tc.name, after.Attempts, before.Attempts+1)
				}
			} else {
				if after.Status != tc.status {
					t.Errorf("status after %s redelivery = %v, want %v (not demoted)", tc.name, after.Status, tc.status)
				}
				if after.Attempts != before.Attempts {
					t.Errorf("attempts after %s redelivery = %d, want unchanged %d", tc.name, after.Attempts, before.Attempts)
				}
			}
		})
	}
}

// TestUpdateTaskRunningSignalsTerminal pins the M3 signal: a committed-terminal
// (success/cancelled) task returns alreadyTerminal=true with NO write (Attempts
// not bumped, status unchanged); a pending task returns false and advances to
// running.
func TestUpdateTaskRunningSignalsTerminal(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	// success task
	ps := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, ps, testJobArgs{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: ps.ID, Status: store.TaskStatusSuccess}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.TaskByID(ctx, ps.ID)
	terminal, err := s.UpdateTaskRunning(ctx, ps.ID)
	if err != nil || !terminal {
		t.Fatalf("UpdateTaskRunning(success) = %v,%v; want true,nil", terminal, err)
	}
	after, _ := s.TaskByID(ctx, ps.ID)
	if after.Attempts != before.Attempts || after.Status != store.TaskStatusSuccess {
		t.Errorf("terminal task mutated: %+v -> %+v", before, after)
	}
	// pending task
	pp := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, pp, testJobArgs{}); err != nil {
		t.Fatal(err)
	}
	terminal, err = s.UpdateTaskRunning(ctx, pp.ID)
	if err != nil || terminal {
		t.Fatalf("UpdateTaskRunning(pending) = %v,%v; want false,nil", terminal, err)
	}
	got, _ := s.TaskByID(ctx, pp.ID)
	if got.Status != store.TaskStatusRunning || got.Attempts != 1 {
		t.Errorf("pending not advanced: %+v", got)
	}
}

func TestUpdateTaskFinalizedWritesTerminal(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	result := []byte(`{"vm_id":"x"}`)
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     p.ID,
		Status: store.TaskStatusSuccess,
		Result: result,
	}); err != nil {
		t.Fatalf("UpdateTaskFinalized: %v", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	if got.Status != store.TaskStatusSuccess || string(got.Result) != string(result) || got.FinishedAt == nil {
		t.Errorf("finalized = (status=%v result=%s finished_at=%v), want success/result/non-nil",
			got.Status, got.Result, got.FinishedAt)
	}

	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{
		ID:     uuid.New(),
		Status: store.TaskStatusFailed,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskFinalized(missing) = %v, want store.ErrNotFound", err)
	}
}

func TestUpdateTaskAgentTaskID(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	agentTaskID := uuid.New()
	if err := s.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
		ID:          p.ID,
		AgentTaskID: &agentTaskID,
	}); err != nil {
		t.Fatalf("UpdateTaskAgentTaskID: %v", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	if got.AgentTaskID == nil || *got.AgentTaskID != agentTaskID {
		t.Errorf("agent_task_id = %v, want %v", got.AgentTaskID, agentTaskID)
	}

	if err := s.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
		ID:          uuid.New(),
		AgentTaskID: &agentTaskID,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskAgentTaskID(missing) = %v, want store.ErrNotFound", err)
	}
}

func TestClearTaskAgentTaskID(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusRunning, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	agentTaskID := uuid.New()
	if err := s.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{
		ID:          p.ID,
		AgentTaskID: &agentTaskID,
	}); err != nil {
		t.Fatalf("UpdateTaskAgentTaskID: %v", err)
	}

	if err := s.ClearTaskAgentTaskID(ctx, p.ID); err != nil {
		t.Fatalf("ClearTaskAgentTaskID: %v", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	if got.AgentTaskID != nil {
		t.Errorf("agent_task_id = %v, want nil after clear", got.AgentTaskID)
	}

	if err := s.ClearTaskAgentTaskID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ClearTaskAgentTaskID(missing) = %v, want store.ErrNotFound", err)
	}
}
