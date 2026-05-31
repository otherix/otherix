// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/etcdstore"
)

// jobSourceFake is an in-memory JobSource for dispatcher unit tests. Safe for
// concurrent handler goroutines.
type jobSourceFake struct {
	mu        sync.Mutex
	jobs      map[int64]etcdstore.Job
	completed map[int64]bool
	retried   map[int64]int32 // id -> last maxAttempts passed to RetryJob
}

func newJobSourceFake() *jobSourceFake {
	return &jobSourceFake{
		jobs:      make(map[int64]etcdstore.Job),
		completed: make(map[int64]bool),
		retried:   make(map[int64]int32),
	}
}

func (f *jobSourceFake) add(j etcdstore.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.ID] = j
}

func (f *jobSourceFake) PendingJobs(context.Context) ([]etcdstore.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []etcdstore.Job
	for _, j := range f.jobs {
		if j.State == etcdstore.JobStatePending {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *jobSourceFake) ClaimJob(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.State != etcdstore.JobStatePending {
		return false, nil
	}
	j.State = etcdstore.JobStateRunning
	f.jobs[id] = j
	return true, nil
}

func (f *jobSourceFake) CompleteJob(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed[id] = true
	delete(f.jobs, id)
	return nil
}

func (f *jobSourceFake) RetryJob(_ context.Context, id int64, maxAttempts int32) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried[id] = maxAttempts
	j := f.jobs[id]
	j.Attempts++
	if j.Attempts < maxAttempts {
		j.State = etcdstore.JobStatePending
	} else {
		j.State = etcdstore.JobStateFailed
	}
	f.jobs[id] = j
	return j.State == etcdstore.JobStatePending, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDispatcherRunsAndCompletes(t *testing.T) {
	src := newJobSourceFake()
	src.add(etcdstore.Job{ID: 1, Kind: "test.job", Args: []byte(`{"x":1}`), State: etcdstore.JobStatePending})

	var (
		mu      sync.Mutex
		gotArgs []byte
	)
	d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
	d.Register("test.job", 3, func(_ context.Context, args []byte) error {
		mu.Lock()
		gotArgs = args
		mu.Unlock()
		return nil
	})

	d.drain(context.Background())
	d.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if string(gotArgs) != `{"x":1}` {
		t.Errorf("handler args = %s, want {\"x\":1}", gotArgs)
	}
	if !src.completed[1] {
		t.Errorf("job 1 not completed")
	}
}

func TestDispatcherUnknownKindIgnored(t *testing.T) {
	src := newJobSourceFake()
	src.add(etcdstore.Job{ID: 7, Kind: "other.job", State: etcdstore.JobStatePending})
	d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
	d.Register("test.job", 3, func(context.Context, []byte) error { return nil })

	d.drain(context.Background())
	d.wg.Wait()

	if src.completed[7] {
		t.Errorf("unknown-kind job was consumed")
	}
	pending, _ := src.PendingJobs(context.Background())
	if len(pending) != 1 {
		t.Errorf("unknown-kind job not left pending: %d", len(pending))
	}
}

func TestDispatcherRetriesOnError(t *testing.T) {
	src := newJobSourceFake()
	src.add(etcdstore.Job{ID: 2, Kind: "test.job", State: etcdstore.JobStatePending})
	d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
	d.Register("test.job", 5, func(context.Context, []byte) error {
		return context.DeadlineExceeded // any error
	})

	d.drain(context.Background())
	d.wg.Wait()

	src.mu.Lock()
	defer src.mu.Unlock()
	if src.completed[2] {
		t.Errorf("failed job wrongly completed")
	}
	if src.retried[2] != 5 {
		t.Errorf("RetryJob maxAttempts = %d, want 5", src.retried[2])
	}
}
