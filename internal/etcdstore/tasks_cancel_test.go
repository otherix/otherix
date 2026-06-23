// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// TestCancelLosesToClaim drives the REAL cancel/claim seam: a job claimed by the
// dispatcher BEFORE cancel cannot be cancelled - the destructive op is already in
// flight. Cancel must return ErrTaskNotCancellable and must NOT flip the task to
// cancelled.
func TestCancelLosesToClaim(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	task, _ := s.TaskByID(ctx, p.ID)
	claimed, err := s.ClaimJob(ctx, *task.JobID) // dispatcher wins first
	if err != nil || !claimed {
		t.Fatalf("ClaimJob = %v, %v; want true, nil", claimed, err)
	}
	if _, err := s.CancelPendingTask(ctx, p.ID, task.JobID); !errors.Is(err, store.ErrTaskNotCancellable) {
		t.Fatalf("cancel after claim = %v, want ErrTaskNotCancellable", err)
	}
	got, _ := s.TaskByID(ctx, p.ID)
	if got.Status == store.TaskStatusCancelled {
		t.Errorf("task was cancelled despite the job being claimed - the race is still open")
	}
}

// TestCancelBeforeClaimDeletesJob: cancel winning before any claim deletes the
// job, so the dispatcher (PendingJobs) never delivers it, and a later ClaimJob
// returns false.
func TestCancelBeforeClaimDeletesJob(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	task, _ := s.TaskByID(ctx, p.ID)
	cancelled, err := s.CancelPendingTask(ctx, p.ID, task.JobID)
	if err != nil {
		t.Fatalf("CancelPendingTask: %v", err)
	}
	if cancelled.Status != store.TaskStatusCancelled || cancelled.FinishedAt == nil {
		t.Errorf("cancelled = %+v, want cancelled + finished_at", cancelled)
	}
	pending, _ := s.PendingJobs(ctx)
	for _, j := range pending {
		if j.ID == *task.JobID {
			t.Errorf("cancelled job %d still pending - dispatcher could deliver a cancelled task", j.ID)
		}
	}
	claimed, _ := s.ClaimJob(ctx, *task.JobID)
	if claimed {
		t.Errorf("ClaimJob succeeded on a cancelled (deleted) job")
	}
}

// TestCancelClaimRaceExactlyOneWins: N goroutines race cancel vs claim on the
// same job; exactly one of {cancelled, claimed} commits, never both.
func TestCancelClaimRaceExactlyOneWins(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		p := taskParams(store.TaskStatusPending, nil)
		if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
			t.Fatalf("EnqueueTask: %v", err)
		}
		task, _ := s.TaskByID(ctx, p.ID)
		var wg sync.WaitGroup
		var cancelOK, claimOK atomic.Bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.CancelPendingTask(ctx, p.ID, task.JobID); err == nil {
				cancelOK.Store(true)
			}
		}()
		go func() {
			defer wg.Done()
			if ok, _ := s.ClaimJob(ctx, *task.JobID); ok {
				claimOK.Store(true)
			}
		}()
		wg.Wait()
		if cancelOK.Load() == claimOK.Load() {
			t.Fatalf("iter %d: cancelOK=%v claimOK=%v - want exactly one", i, cancelOK.Load(), claimOK.Load())
		}
	}
}
