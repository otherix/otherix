// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

// claimFenceRetries bounds the CAS-retry a fenced bookkeeping write makes when it
// races the caller's OWN lease renewer (the only concurrent writer of a running
// job during bookkeeping: the reaper is excluded by the fresh lease, cancel
// touches pending-only, retention failed-only). The renewer fires at most once
// per JobLeaseRenewInterval, so the worst case is 2 rounds; 3 leaves margin.
const claimFenceRetries = 3

// Consumer side of the etcd job queue, driving the worker runtime. Jobs are
// claimed (pending -> running) with a mod-revision compare so two replicas never
// run the same job, completed by deletion, and retried by bumping attempts back
// to pending until the per-kind budget is exhausted (then failed).

func jobsPrefix() string { return etcd.Key("jobs") + "/" }

// PendingJobs returns the jobs currently awaiting a worker, oldest first (the
// zero-padded sequence makes lexical key order numeric order).
func (s *Store) PendingJobs(ctx context.Context) ([]Job, error) {
	items, err := s.c.Range(ctx, jobsPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(items))
	for _, kv := range items {
		var j Job
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &j, "job") {
			continue
		}
		if j.State == JobStatePending {
			out = append(out, j)
		}
	}
	return out, nil
}

// ClaimJob transitions a pending job to running under a mod-revision compare, so
// exactly one worker wins. It mints a fresh claim token and returns it; the
// caller threads it into RenewJobLease/RetryJob/RequeueJob so its bookkeeping
// only ever touches the claim it took. Returns claimed=false (empty token) when
// the job is missing, already claimed, or the compare lost the race.
func (s *Store) ClaimJob(ctx context.Context, id int64) (bool, string, error) {
	job, modRev, found, err := s.jobWithRev(ctx, id)
	if err != nil {
		return false, "", err
	}
	if !found || job.State != JobStatePending {
		return false, "", nil
	}
	token := uuid.NewString()
	job.State = JobStateRunning
	now := time.Now().UTC()
	job.ClaimedAt = &now
	job.ClaimToken = token
	val, err := etcd.Marshal(job)
	if err != nil {
		return false, "", err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(jobKey(id)), "=", modRev)).
		Then(clientv3.OpPut(jobKey(id), string(val))).
		Commit()
	if err != nil {
		return false, "", fmt.Errorf("claim job %d: %v", id, err)
	}
	if !resp.Succeeded {
		return false, "", nil
	}
	return true, token, nil
}

// RenewJobLease refreshes a running job's lease (ClaimedAt = now) under a
// mod-revision compare, so it never stomps a concurrent reclaim or completion.
// The token fences the renew to the caller's own claim: a job reclaimed and
// re-claimed by another worker carries a different token, so the stale renewer
// stops instead of refreshing a lease it no longer owns. Returns true when the
// lease was renewed; false (the renewer must stop) when the job is missing, no
// longer running, not the caller's claim, or the compare lost the race.
func (s *Store) RenewJobLease(ctx context.Context, id int64, token string) (bool, error) {
	job, modRev, found, err := s.jobWithRev(ctx, id)
	if err != nil {
		return false, err
	}
	if !found || job.State != JobStateRunning || token == "" || job.ClaimToken != token {
		return false, nil
	}
	now := time.Now().UTC()
	job.ClaimedAt = &now
	val, err := etcd.Marshal(job)
	if err != nil {
		return false, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(jobKey(id)), "=", modRev)).
		Then(clientv3.OpPut(jobKey(id), string(val))).
		Commit()
	if err != nil {
		return false, fmt.Errorf("renew job lease %d: %v", id, err)
	}
	return resp.Succeeded, nil
}

// CompleteJob deletes a finished job. A missing job is a no-op.
func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	if _, err := s.c.Delete(ctx, jobKey(id)); err != nil {
		return fmt.Errorf("complete job %d: %v", id, err)
	}
	return nil
}

// RetryJob bumps a job's attempt counter and requeues it (state -> pending) when
// the count is still below maxAttempts, or marks it failed otherwise. Returns
// whether the job was requeued. token fences the write to the caller's claim:
// only the running job the caller still holds (matching ClaimToken) is retried,
// so a lease-lost worker cannot stomp a reclaimed+re-claimed job. The write CASes
// on the job's mod-revision with a bounded retry (the caller's own renewer is the
// only competing writer of a running job).
func (s *Store) RetryJob(ctx context.Context, id int64, token string, maxAttempts int32) (bool, error) {
	for i := 0; i < claimFenceRetries; i++ {
		job, modRev, found, err := s.jobWithRev(ctx, id)
		if err != nil {
			return false, err
		}
		if !found || job.State != JobStateRunning || token == "" || job.ClaimToken != token {
			// Not ours: the job was reclaimed and re-claimed by another worker,
			// already resolved (completed/retried/cancelled-deleted), or an empty
			// token (never matches). Never resurrect or stomp it. Fail toward inaction.
			return false, nil
		}
		job.Attempts++
		requeued := job.Attempts < maxAttempts
		if requeued {
			job.State = JobStatePending
			job.ClaimedAt = nil // a pending row carries no live lease
			job.ClaimToken = "" // ...nor a live claim
		} else {
			job.State = JobStateFailed
			now := time.Now().UTC()
			job.FailedAt = &now
			job.ClaimToken = ""
		}
		val, err := etcd.Marshal(job)
		if err != nil {
			return false, err
		}
		resp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(jobKey(id)), "=", modRev)).
			Then(clientv3.OpPut(jobKey(id), string(val))).
			Commit()
		if err != nil {
			return false, fmt.Errorf("retry job %d: %v", id, err)
		}
		if resp.Succeeded {
			return requeued, nil
		}
		// Lost the CAS to the caller's own renewer (token unchanged): re-read + retry.
	}
	return false, fmt.Errorf("retry job %d: claim fence exhausted after %d attempts", id, claimFenceRetries)
}

// RequeueJob returns a running job to pending WITHOUT bumping its attempt
// counter, used when a graceful shutdown aborted the handler mid-flight (a
// deploy is not a real failure, so it must not consume the retry budget). token
// fences the write to the caller's claim (as RetryJob): a job that is not the
// caller's live running claim (reclaimed+re-claimed, already resolved, or empty
// token) is left untouched, so requeueing cannot resurrect finished work or stomp
// another worker's claim. CASes on the mod-revision with a bounded retry.
func (s *Store) RequeueJob(ctx context.Context, id int64, token string) error {
	for i := 0; i < claimFenceRetries; i++ {
		job, modRev, found, err := s.jobWithRev(ctx, id)
		if err != nil {
			return err
		}
		if !found || job.State != JobStateRunning || token == "" || job.ClaimToken != token {
			return nil
		}
		job.State = JobStatePending
		job.ClaimedAt = nil // a pending row carries no live lease
		job.ClaimToken = "" // ...nor a live claim
		val, err := etcd.Marshal(job)
		if err != nil {
			return err
		}
		resp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(jobKey(id)), "=", modRev)).
			Then(clientv3.OpPut(jobKey(id), string(val))).
			Commit()
		if err != nil {
			return fmt.Errorf("requeue job %d: %v", id, err)
		}
		if resp.Succeeded {
			return nil
		}
	}
	return fmt.Errorf("requeue job %d: claim fence exhausted after %d attempts", id, claimFenceRetries)
}

// WatchJobs returns a watch channel over the job prefix so a dispatcher can wake
// promptly on newly enqueued work instead of polling tightly.
func (s *Store) WatchJobs(ctx context.Context) clientv3.WatchChan {
	return s.c.Watch(ctx, jobsPrefix())
}

// jobWithRev reads a job and its current mod-revision (the claim compare target).
func (s *Store) jobWithRev(ctx context.Context, id int64) (Job, int64, bool, error) {
	resp, err := s.c.Raw().Get(ctx, jobKey(id))
	if err != nil {
		return Job{}, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return Job{}, 0, false, nil
	}
	var j Job
	if err := json.Unmarshal(resp.Kvs[0].Value, &j); err != nil {
		return Job{}, 0, false, fmt.Errorf("unmarshal job %d: %v", id, err)
	}
	return j, resp.Kvs[0].ModRevision, true, nil
}
