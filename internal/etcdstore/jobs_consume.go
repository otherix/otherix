// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

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
// exactly one worker wins. Returns false when the job is missing, already
// claimed, or the compare lost the race.
func (s *Store) ClaimJob(ctx context.Context, id int64) (bool, error) {
	job, modRev, found, err := s.jobWithRev(ctx, id)
	if err != nil {
		return false, err
	}
	if !found || job.State != JobStatePending {
		return false, nil
	}
	job.State = JobStateRunning
	val, err := etcd.Marshal(job)
	if err != nil {
		return false, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(jobKey(id)), "=", modRev)).
		Then(clientv3.OpPut(jobKey(id), string(val))).
		Commit()
	if err != nil {
		return false, fmt.Errorf("claim job %d: %v", id, err)
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
// whether the job was requeued. The job is in 'running' (claimed by this
// worker), so the read-modify-write has no competing writer.
func (s *Store) RetryJob(ctx context.Context, id int64, maxAttempts int32) (bool, error) {
	var job Job
	found, err := s.c.GetJSON(ctx, jobKey(id), &job)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if job.State != JobStateRunning {
		// Defense-in-depth: only a running job (claimed by this worker) is ours to
		// retry. A job that is no longer running was resolved out from under us
		// (requeued, completed, cancelled-deleted); never resurrect it.
		return false, nil
	}
	job.Attempts++
	requeued := job.Attempts < maxAttempts
	if requeued {
		job.State = JobStatePending
	} else {
		job.State = JobStateFailed
		now := time.Now().UTC()
		job.FailedAt = &now
	}
	if err := s.c.PutJSON(ctx, jobKey(id), job); err != nil {
		return false, fmt.Errorf("retry job %d: %v", id, err)
	}
	return requeued, nil
}

// RequeueJob returns a running job to pending WITHOUT bumping its attempt
// counter, used when a graceful shutdown aborted the handler mid-flight (a
// deploy is not a real failure, so it must not consume the retry budget). A job
// that is not currently running is left untouched - it was already resolved
// (completed, retried, or cancelled-deleted), so requeueing it would resurrect
// finished work.
func (s *Store) RequeueJob(ctx context.Context, id int64) error {
	var job Job
	found, err := s.c.GetJSON(ctx, jobKey(id), &job)
	if err != nil {
		return err
	}
	if !found || job.State != JobStateRunning {
		return nil
	}
	job.State = JobStatePending
	if err := s.c.PutJSON(ctx, jobKey(id), job); err != nil {
		return fmt.Errorf("requeue job %d: %v", id, err)
	}
	return nil
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
