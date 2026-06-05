// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Tasks are the CP-side handle to background work: a task row is written
// together with its backing job in one transaction (EnqueueTask), so a task is
// never visible without its job. The collection is bounded by per-state
// retention (7d/30d), so listing is a primary-prefix scan with in-app filters.

func taskKey(id uuid.UUID) string { return etcd.Key("tasks", id.String()) }

func taskPrefix() string { return etcd.Key("tasks") + "/" }

// TaskByID returns the task with the given id, or store.ErrNotFound.
func (s *Store) TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error) {
	var t store.Task
	found, err := s.c.GetJSON(ctx, taskKey(id), &t)
	if err != nil {
		return store.Task{}, err
	}
	if !found {
		return store.Task{}, store.ErrNotFound
	}
	return t, nil
}

// taskFromParams builds a fresh task row from create params, stamping
// created_at and the river-equivalent job reference.
func taskFromParams(p store.CreateTaskParams, jobSeq int64) store.Task {
	ref := jobSeq
	return store.Task{
		ID:           p.ID,
		Type:         p.Type,
		Status:       p.Status,
		ResourceType: p.ResourceType,
		ResourceID:   p.ResourceID,
		Args:         p.Args,
		MaxAttempts:  p.MaxAttempts,
		RiverJobID:   &ref,
		CreatedBy:    p.CreatedBy,
		CreatedAt:    time.Now().UTC(),
	}
}

// EnqueueTask writes a task row and its backing job in one transaction, stamping
// the job reference onto the task. Returns params.ID on success. This is the
// producer-side seam every async resource uses.
func (s *Store) EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error) {
	seq, jobOp, err := s.enqueueJobOp(ctx, args)
	if err != nil {
		return uuid.Nil, err
	}
	task := taskFromParams(params, seq)
	val, err := etcd.Marshal(task)
	if err != nil {
		return uuid.Nil, err
	}
	ops := append(taskIndexOps(task), clientv3.OpPut(taskKey(task.ID), string(val)), jobOp)
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("enqueue task txn: %v", err)
	}
	return params.ID, nil
}

// taskIndexOps returns the index writes for a task. Tasks list by a bounded
// primary scan, so the only index maintained is the created-by index used by
// the user-delete cascade and own-scope listing fast path (future).
func taskIndexOps(t store.Task) []clientv3.Op {
	if t.CreatedBy == nil {
		return nil
	}
	return []clientv3.Op{clientv3.OpPut(etcd.Key("index", "tasks", "created_by", t.CreatedBy.String(), t.ID.String()), t.ID.String())}
}

// CancelPendingTask cancels a pending task and its backing job atomically: the
// task flips to cancelled and the job (when jobRef is non-nil) is cancelled in
// the same transaction. Returns store.ErrTaskNotCancellable when the task is no
// longer pending, or store.ErrNotFound when it is missing.
func (s *Store) CancelPendingTask(ctx context.Context, id uuid.UUID, jobRef *int64) (store.Task, error) {
	t, err := s.TaskByID(ctx, id)
	if err != nil {
		return store.Task{}, err
	}
	if t.Status != store.TaskStatusPending {
		return store.Task{}, store.ErrTaskNotCancellable
	}
	now := time.Now().UTC()
	t.Status = store.TaskStatusCancelled
	t.FinishedAt = &now
	if jobRef != nil {
		if err := s.cancelJob(ctx, *jobRef); err != nil {
			return store.Task{}, err
		}
	}
	if err := s.c.PutJSON(ctx, taskKey(id), t); err != nil {
		return store.Task{}, err
	}
	return t, nil
}

// updateTask applies mutate to the stored task and writes it back. Returns
// store.ErrNotFound when the task is absent. The read-modify-write is not
// transactional; task rows are single-writer in practice (one worker owns a
// task at a time), so a lost update cannot occur.
func (s *Store) updateTask(ctx context.Context, id uuid.UUID, mutate func(*store.Task)) error {
	t, err := s.TaskByID(ctx, id)
	if err != nil {
		return err
	}
	mutate(&t)
	return s.c.PutJSON(ctx, taskKey(id), t)
}

// UpdateTaskRunning transitions a task pending -> running: it stamps started_at
// on the first transition (coalesced across retries) and increments the attempt
// counter. Worker entry point.
//
// A worker redelivery calls this at the top of every delivery, including
// deliveries of an already-committed task (the agent ACK was lost, the job is
// redelivered after the projection committed). A committed-terminal task
// (success / cancelled) must NEVER regress to running: it would reopen a task
// the operator sees as done and spuriously bump its Attempts counter. So a
// committed-terminal task is skipped entirely - no write, no status regression,
// no Attempts bump. (The projections are themselves idempotent now, so this is
// belt-and-suspenders rather than load-bearing for correctness.) A failed task
// is retryable (failRun finalizes failed but the
// dispatcher requeues the job), so it still transitions to running and bumps
// Attempts on redelivery - this is what makes the fail-then-succeed retry work.
func (s *Store) UpdateTaskRunning(ctx context.Context, id uuid.UUID) error {
	t, err := s.TaskByID(ctx, id)
	if err != nil {
		return err
	}
	if isCommittedTerminal(t.Status) {
		return nil
	}
	t.Status = store.TaskStatusRunning
	if t.StartedAt == nil {
		now := time.Now().UTC()
		t.StartedAt = &now
	}
	t.Attempts++
	return s.c.PutJSON(ctx, taskKey(id), t)
}

// UpdateTaskFinalized writes a task's terminal status (success / failed /
// cancelled), its result / error payloads, and finished_at. Worker exit point.
func (s *Store) UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error {
	return s.updateTask(ctx, arg.ID, func(t *store.Task) {
		t.Status = arg.Status
		t.Result = arg.Result
		t.Error = arg.Error
		now := time.Now().UTC()
		t.FinishedAt = &now
	})
}

// UpdateTaskAgentTaskID stamps the agent-side task id onto the task row,
// backing the worker resumption seam (a CP restart resumes polling the existing
// agent task instead of re-POSTing).
func (s *Store) UpdateTaskAgentTaskID(ctx context.Context, arg store.UpdateTaskAgentTaskIDParams) error {
	return s.updateTask(ctx, arg.ID, func(t *store.Task) {
		t.AgentTaskID = arg.AgentTaskID
	})
}

// ListTasksAny returns tasks matching the optional filters (unscoped), ordered
// by (created_at, id) descending, after the cursor, capped at LimitCount.
func (s *Store) ListTasksAny(ctx context.Context, arg store.ListTasksAnyParams) ([]store.Task, error) {
	return s.listTasks(ctx, taskFilter{
		statusFilter:       arg.StatusFilter,
		typeFilter:         arg.TypeFilter,
		resourceTypeFilter: arg.ResourceTypeFilter,
		resourceIDFilter:   arg.ResourceIDFilter,
		cursorCreatedAt:    arg.CursorCreatedAt,
		cursorID:           arg.CursorID,
		limitCount:         arg.LimitCount,
	})
}

// ListTasksOwn returns tasks created by a single principal matching the
// filters, with the same ordering and pagination as ListTasksAny.
func (s *Store) ListTasksOwn(ctx context.Context, arg store.ListTasksOwnParams) ([]store.Task, error) {
	return s.listTasks(ctx, taskFilter{
		createdBy:          arg.CreatedBy,
		statusFilter:       arg.StatusFilter,
		typeFilter:         arg.TypeFilter,
		resourceTypeFilter: arg.ResourceTypeFilter,
		resourceIDFilter:   arg.ResourceIDFilter,
		cursorCreatedAt:    arg.CursorCreatedAt,
		cursorID:           arg.CursorID,
		limitCount:         arg.LimitCount,
	})
}

// taskFilter is the unified filter surface for ListTasksAny/Own.
type taskFilter struct {
	createdBy          *uuid.UUID
	statusFilter       *string
	typeFilter         *string
	resourceTypeFilter *string
	resourceIDFilter   *uuid.UUID
	cursorCreatedAt    *time.Time
	cursorID           *uuid.UUID
	limitCount         int32
}

func (s *Store) listTasks(ctx context.Context, f taskFilter) ([]store.Task, error) {
	items, err := s.c.Range(ctx, taskPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.Task, 0, len(items))
	for _, kv := range items {
		var t store.Task
		if err := json.Unmarshal(kv.Value, &t); err != nil {
			return nil, fmt.Errorf("unmarshal task %q: %v", kv.Key, err)
		}
		if !taskMatches(t, f) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if n := int(f.limitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// taskMatches reports whether a task passes every active filter and the DESC
// cursor bound.
func taskMatches(t store.Task, f taskFilter) bool {
	if f.createdBy != nil && (t.CreatedBy == nil || *t.CreatedBy != *f.createdBy) {
		return false
	}
	if f.statusFilter != nil && string(t.Status) != *f.statusFilter {
		return false
	}
	if f.typeFilter != nil && t.Type != *f.typeFilter {
		return false
	}
	if f.resourceTypeFilter != nil && t.ResourceType != *f.resourceTypeFilter {
		return false
	}
	if f.resourceIDFilter != nil && (t.ResourceID == nil || *t.ResourceID != *f.resourceIDFilter) {
		return false
	}
	return beforeCursor(t.CreatedAt, t.ID, f.cursorCreatedAt, f.cursorID)
}
