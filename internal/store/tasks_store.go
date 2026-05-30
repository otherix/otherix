// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/queue"
)

// ErrTaskNotCancellable is returned by CancelPendingTask when the task
// is no longer pending by the time the cancel transaction runs (a
// worker won the race and moved it to running, or it already
// finalized).
var ErrTaskNotCancellable = errors.New("store: task not cancellable")

// TaskByID returns the task with the given id, or ErrNotFound.
func (s *Store) TaskByID(ctx context.Context, id uuid.UUID) (Task, error) {
	t, err := s.queries.GetTask(ctx, id)
	if err != nil {
		return Task{}, translateNoRows(err)
	}
	return t, nil
}

// ListTasksAny returns tasks matching the filters, unscoped (admin /
// operator).
func (s *Store) ListTasksAny(ctx context.Context, arg ListTasksAnyParams) ([]Task, error) {
	return s.queries.ListTasksAny(ctx, arg)
}

// ListTasksOwn returns tasks created by a single principal (scope=own).
func (s *Store) ListTasksOwn(ctx context.Context, arg ListTasksOwnParams) ([]Task, error) {
	return s.queries.ListTasksOwn(ctx, arg)
}

// CancelPendingTask cancels a pending task atomically: it cancels the
// backing background job (when jobRef is non-nil) and flips the task row
// to cancelled in the same transaction. If the row is no longer pending
// when the transaction runs, both writes roll back and
// ErrTaskNotCancellable is returned. The job and the row commit or
// unwind together.
func (s *Store) CancelPendingTask(ctx context.Context, id uuid.UUID, jobRef *int64) (Task, error) {
	var out Task
	err := s.InTxEnqueue(ctx, func(q *Queries, enq queue.Enqueuer) error {
		if jobRef != nil {
			if err := enq.Cancel(ctx, queue.JobRef{ID: *jobRef}); err != nil {
				return err
			}
		}
		cancelled, err := q.CancelTaskIfPending(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTaskNotCancellable
			}
			return err
		}
		out = cancelled
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}
