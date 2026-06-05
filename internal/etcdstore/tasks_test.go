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
	if got.Status != store.TaskStatusPending || got.RiverJobID == nil || *got.RiverJobID < 1 {
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
		if *got.RiverJobID <= prev {
			t.Errorf("job seq not monotonic: %d after %d", *got.RiverJobID, prev)
		}
		prev = *got.RiverJobID
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
	cancelled, err := s.CancelPendingTask(ctx, p.ID, got.RiverJobID)
	if err != nil {
		t.Fatalf("CancelPendingTask: %v", err)
	}
	if cancelled.Status != store.TaskStatusCancelled || cancelled.FinishedAt == nil {
		t.Errorf("cancelled = %+v, want cancelled + finished_at", cancelled)
	}
	// Second cancel: no longer pending.
	if _, err := s.CancelPendingTask(ctx, p.ID, got.RiverJobID); !errors.Is(err, store.ErrTaskNotCancellable) {
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

	if err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
		t.Fatalf("UpdateTaskRunning: %v", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	if got.Status != store.TaskStatusRunning || got.Attempts != 1 || got.StartedAt == nil {
		t.Errorf("after first run: status=%v attempts=%d started_at=%v, want running/1/non-nil",
			got.Status, got.Attempts, got.StartedAt)
	}
	firstStarted := *got.StartedAt

	// Second transition: attempts++ but started_at is coalesced (unchanged).
	if err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
		t.Fatalf("UpdateTaskRunning (retry): %v", err)
	}
	got, _ = s.TaskByID(ctx, p.ID)
	if got.Attempts != 2 || got.StartedAt == nil || !got.StartedAt.Equal(firstStarted) {
		t.Errorf("after retry: attempts=%d started_at=%v, want 2 and unchanged %v",
			got.Attempts, got.StartedAt, firstStarted)
	}

	// Missing task.
	if err := s.UpdateTaskRunning(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
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
			if err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
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
			if err := s.UpdateTaskRunning(ctx, p.ID); err != nil {
				t.Fatalf("UpdateTaskRunning (redelivery) = %v, want nil", err)
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
