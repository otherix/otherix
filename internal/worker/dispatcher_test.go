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
	requeued  map[int64]int   // id -> RequeueJob call count
	renewed   map[int64]int   // id -> RenewJobLease call count
}

func newJobSourceFake() *jobSourceFake {
	return &jobSourceFake{
		jobs:      make(map[int64]etcdstore.Job),
		completed: make(map[int64]bool),
		retried:   make(map[int64]int32),
		requeued:  make(map[int64]int),
		renewed:   make(map[int64]int),
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

func (f *jobSourceFake) RequeueJob(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requeued[id]++
	j := f.jobs[id]
	j.State = etcdstore.JobStatePending
	f.jobs[id] = j
	return nil
}

func (f *jobSourceFake) RenewJobLease(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed[id]++
	j, ok := f.jobs[id]
	return ok && j.State == etcdstore.JobStateRunning, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestExecuteRenewsLeaseWhileBlocked drives the REAL execute path with a blocking
// handler and a short renew interval: the dispatcher must renew the job's lease at
// least once while the handler is blocked, and stop renewing once it returns.
func TestExecuteRenewsLeaseWhileBlocked(t *testing.T) {
	src := newJobSourceFake()
	src.add(etcdstore.Job{ID: 20, Kind: "test.job", State: etcdstore.JobStateRunning})

	release := make(chan struct{})
	reg := registration{
		handler: func(context.Context, []byte) error {
			<-release
			return nil
		},
		maxAttempts: 5,
	}

	d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
	d.renewInterval = 5 * time.Millisecond

	done := make(chan struct{})
	go func() {
		d.execute(context.Background(), etcdstore.Job{ID: 20, Kind: "test.job"}, reg)
		close(done)
	}()

	// While blocked, the renewer must fire at least once.
	deadline := time.Now().Add(2 * time.Second)
	for {
		src.mu.Lock()
		count := src.renewed[20]
		src.mu.Unlock()
		if count >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("RenewJobLease never called while handler blocked")
		}
		time.Sleep(time.Millisecond)
	}

	close(release)
	<-done

	// Renewals stop after the handler returns: record the count, wait a few
	// intervals, and assert it did not grow.
	src.mu.Lock()
	stable := src.renewed[20]
	src.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	src.mu.Lock()
	after := src.renewed[20]
	src.mu.Unlock()
	if after != stable {
		t.Errorf("RenewJobLease kept firing after handler returned: %d -> %d", stable, after)
	}
}

// renewStopSourceFake is a JobSource whose RenewJobLease returns false after the
// first call (modelling a concurrent reclaim that won the job out from under the
// renewer). Every renew call is signalled on renewCh so the test can synchronise
// without sleeps. Only the renewer surface is exercised here; the other methods
// satisfy the interface and are unused.
type renewStopSourceFake struct {
	mu      sync.Mutex
	calls   int
	renewCh chan struct{}
}

func (f *renewStopSourceFake) PendingJobs(context.Context) ([]etcdstore.Job, error) {
	return nil, nil
}
func (f *renewStopSourceFake) ClaimJob(context.Context, int64) (bool, error) { return false, nil }
func (f *renewStopSourceFake) CompleteJob(context.Context, int64) error      { return nil }
func (f *renewStopSourceFake) RetryJob(context.Context, int64, int32) (bool, error) {
	return false, nil
}
func (f *renewStopSourceFake) RequeueJob(context.Context, int64) error { return nil }

func (f *renewStopSourceFake) RenewJobLease(_ context.Context, _ int64) (bool, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	f.renewCh <- struct{}{}
	// First renew succeeds; every later one reports the lease is no longer ours,
	// which must stop the renewer.
	return n == 1, nil
}

// TestExecuteRenewerStopsOnLostLease drives the REAL execute path and pins the
// renewer's stop-on-RenewJobLease==false branch: when a renew reports the lease
// is no longer ours (a concurrent reclaim won the job), the renewer must STOP
// calling RenewJobLease even though the handler has not returned yet.
func TestExecuteRenewerStopsOnLostLease(t *testing.T) {
	src := &renewStopSourceFake{renewCh: make(chan struct{}, 1)}

	release := make(chan struct{})
	reg := registration{
		handler: func(context.Context, []byte) error {
			<-release // block until the test has observed the renewer stop
			return nil
		},
		maxAttempts: 5,
	}

	d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
	d.renewInterval = time.Millisecond

	done := make(chan struct{})
	go func() {
		d.execute(context.Background(), etcdstore.Job{ID: 30, Kind: "test.job"}, reg)
		close(done)
	}()

	// First renew fires and succeeds.
	<-src.renewCh
	// Second renew fires and returns false -> the renewer must stop. After
	// draining it, no further renew call may ever arrive while the handler is
	// still blocked.
	<-src.renewCh

	// The handler is still blocked (renewInterval is far shorter than this wait),
	// so any third renew would land here. None must.
	select {
	case <-src.renewCh:
		t.Fatalf("RenewJobLease called again after reporting the lease was lost; renewer did not stop")
	case <-time.After(50 * time.Millisecond):
	}

	src.mu.Lock()
	calls := src.calls
	src.mu.Unlock()
	if calls != 2 {
		t.Errorf("RenewJobLease call count = %d, want 2 (renewer stopped after the false return)", calls)
	}

	close(release)
	<-done
}

// TestExecuteRequeuesOnShutdown drives the REAL execute path with a cancelled
// run ctx (the graceful-shutdown signal): a handler that aborts because of the
// cancel must REQUEUE the job (RequeueJob), NOT RetryJob it (no attempt bump). A
// normal handler error with a live ctx still goes through RetryJob.
func TestExecuteRequeuesOnShutdown(t *testing.T) {
	// abortOnCancel returns the ctx error, modelling a handler that aborts when
	// graceful shutdown cancels the run ctx mid-flight.
	abortOnCancel := registration{
		handler:     func(ctx context.Context, _ []byte) error { return ctx.Err() },
		maxAttempts: 5,
	}

	// Shutdown path: ctx cancelled, handler returns ctx.Err() != nil.
	t.Run("shutdown requeues without retry", func(t *testing.T) {
		src := newJobSourceFake()
		src.add(etcdstore.Job{ID: 10, Kind: "test.job", State: etcdstore.JobStateRunning})
		d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d.execute(ctx, etcdstore.Job{ID: 10, Kind: "test.job"}, abortOnCancel)
		src.mu.Lock()
		defer src.mu.Unlock()
		if src.requeued[10] != 1 {
			t.Errorf("RequeueJob calls = %d, want 1 (shutdown path)", src.requeued[10])
		}
		if _, retried := src.retried[10]; retried {
			t.Errorf("RetryJob called on the shutdown path; want RequeueJob only")
		}
		if src.completed[10] {
			t.Errorf("job completed on the shutdown path; want requeued")
		}
	})

	// Normal-error path: ctx live, handler returns a real error -> RetryJob.
	t.Run("live error retries", func(t *testing.T) {
		src := newJobSourceFake()
		src.add(etcdstore.Job{ID: 11, Kind: "test.job", State: etcdstore.JobStateRunning})
		d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)
		boom := registration{handler: func(context.Context, []byte) error { return context.DeadlineExceeded }, maxAttempts: 5}
		d.execute(context.Background(), etcdstore.Job{ID: 11, Kind: "test.job"}, boom)
		src.mu.Lock()
		defer src.mu.Unlock()
		if src.retried[11] != 5 {
			t.Errorf("RetryJob maxAttempts = %d, want 5 (live-error path)", src.retried[11])
		}
		if src.requeued[11] != 0 {
			t.Errorf("RequeueJob called on a live-error path; want RetryJob only")
		}
	})
}

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

// TestExecuteRecoversHandlerPanic drives the real execute path with a handler
// that panics: execute must NOT propagate the panic (which would unwind the
// worker goroutine and crash the whole api-server process, embedded etcd with
// it), and must route the recovered panic through the normal retry path so the
// poison job consumes its attempt budget instead of crash-looping forever.
func TestExecuteRecoversHandlerPanic(t *testing.T) {
	src := newJobSourceFake()
	src.add(etcdstore.Job{ID: 30, Kind: "poison", State: etcdstore.JobStateRunning})
	reg := registration{
		handler:     func(context.Context, []byte) error { panic("boom") },
		maxAttempts: 5,
	}
	d := NewDispatcher(src, discardLogger(), time.Millisecond, 4)

	// Must return normally (recovered), not panic out of execute.
	d.execute(context.Background(), etcdstore.Job{ID: 30, Kind: "poison"}, reg)

	src.mu.Lock()
	_, retried := src.retried[30]
	src.mu.Unlock()
	if !retried {
		t.Errorf("panicking handler did not go through RetryJob: attempt budget never consumed, poison job would crash-loop")
	}
}
