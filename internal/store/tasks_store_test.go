// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// noopBinder is a store.QueueBinder whose Enqueuer does nothing, letting
// store tests exercise InTxEnqueue-backed methods without a real queue
// backend. Cancel/Enqueue succeed as no-ops.
type noopBinder struct{}

func (noopBinder) Bind(pgx.Tx) queue.Enqueuer { return noopEnqueuer{} }

type noopEnqueuer struct{}

func (noopEnqueuer) Enqueue(context.Context, queue.JobArgs) (queue.JobRef, error) {
	return queue.JobRef{}, nil
}
func (noopEnqueuer) Cancel(context.Context, queue.JobRef) error { return nil }

func pendingTaskParams(id uuid.UUID) store.CreateTaskParams {
	return store.CreateTaskParams{
		ID:           id,
		Type:         "test.noop",
		Status:       store.TaskStatusPending,
		ResourceType: "template",
		Args:         []byte("{}"),
		MaxAttempts:  1,
	}
}

// importArgsStub is a queue.JobArgs stand-in so EnqueueTask can be
// exercised without importing a handler package (which would create an
// import cycle). Its Kind matches a real task type for realism.
type importArgsStub struct{}

func (importArgsStub) Kind() string { return "storage_image.import" }

func TestEnqueueTask(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	s.SetQueueBinder(noopBinder{})

	id := uuid.New()
	got, err := s.EnqueueTask(ctx, pendingTaskParams(id), importArgsStub{})
	if err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	if got != id {
		t.Errorf("EnqueueTask returned id %v, want %v", got, id)
	}
	row, err := s.TaskByID(ctx, id)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if row.Status != store.TaskStatusPending {
		t.Errorf("task status = %v, want pending", row.Status)
	}
}

func TestTaskByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.TaskByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TaskByID(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestCancelPendingTask(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	s.SetQueueBinder(noopBinder{})

	id := uuid.New()
	if _, err := s.Queries().CreateTask(ctx, pendingTaskParams(id)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cancelled, err := s.CancelPendingTask(ctx, id, nil)
	if err != nil {
		t.Fatalf("CancelPendingTask: %v", err)
	}
	if cancelled.Status != store.TaskStatusCancelled {
		t.Errorf("status = %v, want cancelled", cancelled.Status)
	}

	// Second attempt: no longer pending -> ErrTaskNotCancellable.
	if _, err := s.CancelPendingTask(ctx, id, nil); !errors.Is(err, store.ErrTaskNotCancellable) {
		t.Errorf("re-cancel error = %v, want store.ErrTaskNotCancellable", err)
	}
}
