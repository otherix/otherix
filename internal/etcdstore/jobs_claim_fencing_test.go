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

// reclaimAndReClaim drives a job through the reaper (running -> pending, token
// cleared) and a fresh claim (pending -> running, new token), modelling a
// lease-lost worker whose job was picked up by another. It returns the two claim
// tokens; they must differ.
func reclaimAndReClaim(t *testing.T, s *etcdstore.Store, id int64, tokenA string) string {
	t.Helper()
	ctx := context.Background()
	// olderThan in the future makes every running job's lease look stale, so the
	// reaper reclaims regardless of the real ClaimedAt age.
	if _, err := s.ReclaimStaleRunningJobs(ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("ReclaimStaleRunningJobs: %v", err)
	}
	claimed, tokenB, err := s.ClaimJob(ctx, id)
	if err != nil || !claimed {
		t.Fatalf("re-ClaimJob = (%v, %v), want claimed", claimed, err)
	}
	if tokenB == "" || tokenB == tokenA {
		t.Fatalf("re-claim token %q must be non-empty and differ from %q; test is toothless", tokenB, tokenA)
	}
	return tokenB
}

// enqueueAndClaim enqueues one job and claims it, returning the job id and token.
func enqueueAndClaim(t *testing.T, s *etcdstore.Store) (int64, string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.EnqueueTask(ctx, taskParams(store.TaskStatusPending, nil), testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	job := mustPending(t, s)[0]
	claimed, token, err := s.ClaimJob(ctx, job.ID)
	if err != nil || !claimed {
		t.Fatalf("ClaimJob = (%v, %v), want claimed", claimed, err)
	}
	return job.ID, token
}

// jobByID reads a job row directly (a running row is not in PendingJobs).
func jobByID(t *testing.T, cli *etcd.Client, id int64) etcdstore.Job {
	t.Helper()
	var j etcdstore.Job
	found, err := cli.GetJSON(context.Background(), etcd.Key("jobs", fmt.Sprintf("%020d", id)), &j)
	if err != nil || !found {
		t.Fatalf("read job %d = (found=%v, %v)", id, found, err)
	}
	return j
}

// TestRetryJobFencesStaleClaim is the core claim-fence teeth: a lease-lost worker
// A, whose job was reclaimed and re-claimed by B, must NOT stomp B's claim when
// its late handler failure calls RetryJob with A's stale token. The job must stay
// exactly as B left it (running, B's token, attempts unchanged).
func TestRetryJobFencesStaleClaim(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	id, tokenA := enqueueAndClaim(t, s)
	tokenB := reclaimAndReClaim(t, s, id, tokenA)

	// A's late RetryJob with the STALE token: must no-op.
	requeued, err := s.RetryJob(ctx, id, tokenA, 5)
	if err != nil {
		t.Fatalf("RetryJob(stale token) = %v, want nil", err)
	}
	if requeued {
		t.Errorf("RetryJob(stale token) requeued a re-claimed job; the fence must skip it")
	}
	got := jobByID(t, cli, id)
	if got.State != etcdstore.JobStateRunning || got.ClaimToken != tokenB || got.Attempts != 0 {
		t.Errorf("job after stale RetryJob = {state:%s token:%s attempts:%d}, want {running %s 0}", got.State, got.ClaimToken, got.Attempts, tokenB)
	}

	// B's RetryJob with the CURRENT token still works (requeues, attempts=1).
	requeued, err = s.RetryJob(ctx, id, tokenB, 5)
	if err != nil || !requeued {
		t.Fatalf("RetryJob(current token) = (%v, %v), want requeued", requeued, err)
	}
	after := mustPending(t, s)
	if len(after) != 1 || after[0].ID != id || after[0].Attempts != 1 {
		t.Errorf("job after current RetryJob = %+v, want pending id=%d attempts=1", after, id)
	}
}

// TestRequeueJobFencesStaleClaim mirrors the RetryJob fence for the graceful-
// shutdown requeue path: a stale token must not return a re-claimed job to
// pending.
func TestRequeueJobFencesStaleClaim(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	id, tokenA := enqueueAndClaim(t, s)
	tokenB := reclaimAndReClaim(t, s, id, tokenA)

	if err := s.RequeueJob(ctx, id, tokenA); err != nil {
		t.Fatalf("RequeueJob(stale token) = %v, want nil", err)
	}
	got := jobByID(t, cli, id)
	if got.State != etcdstore.JobStateRunning || got.ClaimToken != tokenB {
		t.Errorf("job after stale RequeueJob = {state:%s token:%s}, want {running %s}", got.State, got.ClaimToken, tokenB)
	}
}

// TestRenewJobLeaseFencesStaleClaim pins that a lease-lost worker's renewer stops
// (returns false) instead of refreshing a job re-claimed by another worker.
func TestRenewJobLeaseFencesStaleClaim(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	id, tokenA := enqueueAndClaim(t, s)
	tokenB := reclaimAndReClaim(t, s, id, tokenA)

	ok, err := s.RenewJobLease(ctx, id, tokenA)
	if err != nil {
		t.Fatalf("RenewJobLease(stale token) = %v, want nil", err)
	}
	if ok {
		t.Errorf("RenewJobLease(stale token) = true; a lease-lost renewer must stop, not refresh B's claim")
	}
	// B's renewer still works.
	if ok, err := s.RenewJobLease(ctx, id, tokenB); err != nil || !ok {
		t.Errorf("RenewJobLease(current token) = (%v, %v), want true", ok, err)
	}
}

// TestFencedMethodsRejectEmptyTokenOnLegacyRow pins the L1 defense-in-depth in
// the scenario that motivates it: a legacy pre-upgrade running row carries an
// empty ClaimToken, and an empty caller token must NOT match it (else a new
// worker passing "" - which it never does - would fence-match every legacy row).
// The row must be left untouched (reaped later by the lease reaper), never
// retried or renewed.
func TestFencedMethodsRejectEmptyTokenOnLegacyRow(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	id, _ := enqueueAndClaim(t, s)

	// Simulate a legacy running row with no claim token.
	j := jobByID(t, cli, id)
	j.ClaimToken = ""
	if err := cli.PutJSON(ctx, etcd.Key("jobs", fmt.Sprintf("%020d", id)), j); err != nil {
		t.Fatalf("seed legacy empty-token row: %v", err)
	}

	if requeued, err := s.RetryJob(ctx, id, "", 5); err != nil || requeued {
		t.Errorf("RetryJob(\"\", legacy empty-token row) = (%v, %v), want (false, nil) no-op", requeued, err)
	}
	if err := s.RequeueJob(ctx, id, ""); err != nil {
		t.Errorf("RequeueJob(\"\", legacy row) = %v, want nil no-op", err)
	}
	if ok, err := s.RenewJobLease(ctx, id, ""); err != nil || ok {
		t.Errorf("RenewJobLease(\"\", legacy row) = (%v, %v), want (false, nil)", ok, err)
	}
	// The legacy row is untouched: still running, no attempt bump.
	got := jobByID(t, cli, id)
	if got.State != etcdstore.JobStateRunning || got.Attempts != 0 {
		t.Errorf("legacy row mutated by an empty-token call = %+v, want running attempts=0", got)
	}
}
