// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

func TestJobConsumeLifecycle(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	// Two enqueued tasks => two pending jobs, oldest first.
	a := taskParams(store.TaskStatusPending, nil)
	b := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, a, testJobArgs{Foo: "a"}); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if _, err := s.EnqueueTask(ctx, b, testJobArgs{Foo: "b"}); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	pending, err := s.PendingJobs(ctx)
	if err != nil {
		t.Fatalf("PendingJobs: %v", err)
	}
	if len(pending) != 2 || pending[0].ID >= pending[1].ID {
		t.Fatalf("pending = %+v, want 2 oldest-first", pending)
	}
	if pending[0].Kind != "test.job" {
		t.Errorf("kind = %q, want test.job", pending[0].Kind)
	}

	first := pending[0].ID
	// Claim transitions pending -> running; a second claim fails.
	claimed, err := s.ClaimJob(ctx, first)
	if err != nil || !claimed {
		t.Fatalf("ClaimJob = (%v, %v), want claimed", claimed, err)
	}
	again, err := s.ClaimJob(ctx, first)
	if err != nil || again {
		t.Errorf("second ClaimJob = (%v, %v), want not claimed", again, err)
	}
	// Claimed job no longer appears as pending.
	pending, _ = s.PendingJobs(ctx)
	if len(pending) != 1 || pending[0].ID == first {
		t.Errorf("pending after claim = %+v, want only the second job", pending)
	}

	// Complete deletes it.
	if err := s.CompleteJob(ctx, first); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	pending, _ = s.PendingJobs(ctx)
	if len(pending) != 1 {
		t.Errorf("pending after complete = %d, want 1", len(pending))
	}
}

func TestJobRetryAndFail(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job := mustPending(t, s)[0]
	if _, err := s.ClaimJob(ctx, job.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// maxAttempts=2: first retry requeues (attempts 1 < 2), it becomes pending again.
	requeued, err := s.RetryJob(ctx, job.ID, 2)
	if err != nil || !requeued {
		t.Fatalf("RetryJob#1 = (%v, %v), want requeued", requeued, err)
	}
	again := mustPending(t, s)
	if len(again) != 1 || again[0].Attempts != 1 {
		t.Fatalf("after retry pending = %+v, want one with attempts=1", again)
	}

	// Claim + retry again: attempts reaches 2 == max => failed, not pending.
	if _, err := s.ClaimJob(ctx, job.ID); err != nil {
		t.Fatalf("claim#2: %v", err)
	}
	requeued, err = s.RetryJob(ctx, job.ID, 2)
	if err != nil || requeued {
		t.Fatalf("RetryJob#2 = (%v, %v), want not requeued (failed)", requeued, err)
	}
	if len(mustPending(t, s)) != 0 {
		t.Errorf("pending after exhausting attempts = %d, want 0", len(mustPending(t, s)))
	}
}

func mustPending(t *testing.T, s *etcdstore.Store) []etcdstore.Job {
	t.Helper()
	jobs, err := s.PendingJobs(context.Background())
	if err != nil {
		t.Fatalf("PendingJobs: %v", err)
	}
	return jobs
}
