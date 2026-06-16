// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/etcd"
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
	// A requeued (pending) row carries no live lease.
	if again[0].ClaimedAt != nil {
		t.Errorf("requeued job ClaimedAt = %v, want nil", again[0].ClaimedAt)
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

// TestRequeueJob pins the graceful-shutdown requeue: a running job returns to
// pending with NO attempt bump; a non-running job is left untouched.
func TestRequeueJob(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatal(err)
	}
	task, _ := s.TaskByID(ctx, p.ID)
	if ok, _ := s.ClaimJob(ctx, *task.RiverJobID); !ok {
		t.Fatal("claim")
	}
	if err := s.RequeueJob(ctx, *task.RiverJobID); err != nil {
		t.Fatalf("RequeueJob: %v", err)
	}
	pending, _ := s.PendingJobs(ctx)
	var found bool
	for _, j := range pending {
		if j.ID == *task.RiverJobID {
			found = true
			if j.Attempts != 0 {
				t.Errorf("attempts bumped on requeue: %d, want 0", j.Attempts)
			}
			// A requeued (pending) row carries no live lease.
			if j.ClaimedAt != nil {
				t.Errorf("requeued job ClaimedAt = %v, want nil", j.ClaimedAt)
			}
		}
	}
	if !found {
		t.Errorf("requeued job not pending again")
	}

	// A second requeue (job now pending, not running) is a no-op: it must not
	// touch the job.
	if err := s.RequeueJob(ctx, *task.RiverJobID); err != nil {
		t.Fatalf("RequeueJob (non-running) = %v, want nil no-op", err)
	}
	pending, _ = s.PendingJobs(ctx)
	var stillFound bool
	for _, j := range pending {
		if j.ID == *task.RiverJobID && j.Attempts == 0 {
			stillFound = true
		}
	}
	if !stillFound {
		t.Errorf("non-running requeue mutated the job")
	}
}

// TestRetryJobOnlyRequeuesRunning pins the RetryJob state guard (defense-in-
// depth): a job that is NOT running (e.g. already requeued to pending) is left
// untouched and reported not-requeued, never resurrected with a bumped attempt.
func TestRetryJobOnlyRequeuesRunning(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatal(err)
	}
	job := mustPending(t, s)[0] // pending, never claimed

	requeued, err := s.RetryJob(ctx, job.ID, 5)
	if err != nil {
		t.Fatalf("RetryJob(pending) = %v", err)
	}
	if requeued {
		t.Errorf("RetryJob requeued a non-running (pending) job; the state guard must skip it")
	}
	after := mustPending(t, s)
	if len(after) != 1 || after[0].Attempts != 0 {
		t.Errorf("RetryJob mutated a non-running job: %+v", after)
	}
}

// TestPendingJobsQuarantinesUndecodableKey pins the L12 quarantine: a malformed
// job key under the jobs prefix must not halt PendingJobs. The valid pending job
// is still returned, no error, and the poison key is left in place (not deleted)
// so an operator can investigate (audit R2-L12).
func TestPendingJobsQuarantinesUndecodableKey(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	// One valid pending job via the normal enqueue path.
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	// A poison key under the jobs prefix carrying raw non-JSON bytes.
	poisonKey := etcd.Key("jobs", "99999999999999999999")
	if _, err := cli.Raw().Put(ctx, poisonKey, "not json"); err != nil {
		t.Fatalf("seed poison key: %v", err)
	}

	got, err := s.PendingJobs(ctx)
	if err != nil {
		t.Fatalf("PendingJobs error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("PendingJobs len = %d, want 1 (valid job, poison skipped)", len(got))
	}

	// The poison key is quarantined (skipped + logged), never deleted.
	resp, err := cli.Raw().Get(ctx, poisonKey)
	if err != nil {
		t.Fatalf("get poison key: %v", err)
	}
	if len(resp.Kvs) != 1 {
		t.Errorf("poison key count = %d, want 1 (must not be deleted)", len(resp.Kvs))
	}
}

// TestClaimJobStampsClaimedAt pins that ClaimJob records a recent ClaimedAt when
// it flips a pending job to running - the timestamp the lease reaper reads.
func TestClaimJobStampsClaimedAt(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	job := mustPending(t, s)[0]
	if job.ClaimedAt != nil {
		t.Errorf("pending job ClaimedAt = %v, want nil", job.ClaimedAt)
	}

	before := time.Now().UTC()
	if ok, err := s.ClaimJob(ctx, job.ID); err != nil || !ok {
		t.Fatalf("ClaimJob = (%v, %v), want claimed", ok, err)
	}

	var got etcdstore.Job
	if _, err := cli.GetJSON(ctx, etcd.Key("jobs", fmt.Sprintf("%020d", job.ID)), &got); err != nil {
		t.Fatalf("read claimed job: %v", err)
	}
	if got.ClaimedAt == nil {
		t.Fatalf("claimed job ClaimedAt = nil, want a recent timestamp")
	}
	if got.ClaimedAt.Before(before) || got.ClaimedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("claimed job ClaimedAt = %v, want ~now", got.ClaimedAt)
	}
}

// TestRenewJobLease pins the worker-side lease renewal: a running job's ClaimedAt
// advances and the call returns true; a pending/un-claimed job and a missing job
// both return false (the renewer must stop).
func TestRenewJobLease(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	readClaimedAt := func(id int64) *time.Time {
		var j etcdstore.Job
		if _, err := cli.GetJSON(ctx, etcd.Key("jobs", fmt.Sprintf("%020d", id)), &j); err != nil {
			t.Fatalf("read job %d: %v", id, err)
		}
		return j.ClaimedAt
	}

	p := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	job := mustPending(t, s)[0]

	// Pending (un-claimed) job: renew is a no-op, returns false.
	ok, err := s.RenewJobLease(ctx, job.ID)
	if err != nil {
		t.Fatalf("RenewJobLease(pending) error = %v", err)
	}
	if ok {
		t.Errorf("RenewJobLease(pending) = true, want false")
	}

	// Missing job: returns false.
	ok, err = s.RenewJobLease(ctx, 9_999_999)
	if err != nil {
		t.Fatalf("RenewJobLease(missing) error = %v", err)
	}
	if ok {
		t.Errorf("RenewJobLease(missing) = true, want false")
	}

	// Claim it (stamps ClaimedAt), then renew advances ClaimedAt and returns true.
	if claimed, err := s.ClaimJob(ctx, job.ID); err != nil || !claimed {
		t.Fatalf("ClaimJob = (%v, %v), want claimed", claimed, err)
	}
	first := readClaimedAt(job.ID)
	if first == nil {
		t.Fatal("ClaimedAt nil after claim")
	}
	time.Sleep(5 * time.Millisecond)
	ok, err = s.RenewJobLease(ctx, job.ID)
	if err != nil {
		t.Fatalf("RenewJobLease(running) error = %v", err)
	}
	if !ok {
		t.Errorf("RenewJobLease(running) = false, want true")
	}
	second := readClaimedAt(job.ID)
	if second == nil || !second.After(*first) {
		t.Errorf("ClaimedAt did not advance: first=%v second=%v", first, second)
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
