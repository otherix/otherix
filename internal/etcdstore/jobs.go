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
	"github.com/otherix/otherix/internal/queue"
)

// Background jobs: each enqueued job is a key under /otherix/jobs/ keyed by a
// monotonic sequence (the int64 job ref persisted on the owning task), so the
// worker runtime can consume them in order. The sequence counter lives at
// /otherix/seq/jobs and is bumped with a
// value-compare CAS loop.

// JobState enumerates a job's lifecycle on the etcd queue.
type JobState string

// Job lifecycle states on the etcd-backed queue. There is no terminal
// "cancelled" job state: a cancel deletes the backing job in the same CAS
// transaction that flips its task to cancelled (see CancelPendingTask), so a
// cancelled task simply has no job - never a cancelled one.
const (
	JobStatePending   JobState = "pending"
	JobStateRunning   JobState = "running"
	JobStateCompleted JobState = "completed"
	JobStateFailed    JobState = "failed"
)

// Job is the persisted unit of background work consumed by the worker runtime.
type Job struct {
	ID       int64      `json:"id"`
	Kind     string     `json:"kind"`
	Args     []byte     `json:"args"`
	State    JobState   `json:"state"`
	Attempts int32      `json:"attempts"`
	FailedAt *time.Time `json:"failed_at,omitempty"`
}

func jobSeqKey() string { return etcd.Key("seq", "jobs") }

// jobKey zero-pads the sequence so lexical key order matches numeric order (the
// worker consumes oldest-first).
func jobKey(seq int64) string { return etcd.Key("jobs", fmt.Sprintf("%020d", seq)) }

// nextJobSeq atomically allocates the next job sequence via a value-compare CAS
// loop on the counter key.
func (s *Store) nextJobSeq(ctx context.Context) (int64, error) {
	return s.nextSeq(ctx, jobSeqKey())
}

// enqueueJobOp allocates a sequence and returns the put op that persists a
// pending job for args, plus the allocated sequence (the job ref). The op is
// composed into the same transaction as the task row so the task and its job
// commit together.
func (s *Store) enqueueJobOp(ctx context.Context, args queue.JobArgs) (int64, clientv3.Op, error) {
	seq, err := s.nextJobSeq(ctx)
	if err != nil {
		return 0, clientv3.Op{}, err
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return 0, clientv3.Op{}, fmt.Errorf("marshal job args: %v", err)
	}
	job := Job{ID: seq, Kind: args.Kind(), Args: payload, State: JobStatePending}
	val, err := etcd.Marshal(job)
	if err != nil {
		return 0, clientv3.Op{}, err
	}
	return seq, clientv3.OpPut(jobKey(seq), string(val)), nil
}
