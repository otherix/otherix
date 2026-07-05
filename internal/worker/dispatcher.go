// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package worker is the etcd-backed control-plane job runtime. The Dispatcher
// consumes the etcd job queue (internal/etcdstore) and routes each job to a
// registered handler, the in-process worker dispatcher
// pool. Task-driven worker logic lives in the owning handler packages as
// Run functions registered here; periodic work is driven separately by the
// Scheduler.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/otherix/otherix/internal/etcdstore"
)

// JobSource is the queue surface the dispatcher consumes. *etcdstore.Store
// satisfies it.
type JobSource interface {
	// PendingJobs returns jobs awaiting a worker, oldest first.
	PendingJobs(ctx context.Context) ([]etcdstore.Job, error)
	// ClaimJob transitions a pending job to running; false means another worker
	// won or the job is gone.
	ClaimJob(ctx context.Context, id int64) (bool, error)
	// CompleteJob removes a finished job.
	CompleteJob(ctx context.Context, id int64) error
	// RetryJob requeues a failed job while under maxAttempts, else fails it;
	// returns whether it was requeued.
	RetryJob(ctx context.Context, id int64, maxAttempts int32) (bool, error)
	// RequeueJob returns a running job to pending without an attempt bump, used
	// on the graceful-shutdown path (a deploy is not a real failure).
	RequeueJob(ctx context.Context, id int64) error
	// RenewJobLease refreshes a running job's lease; false means the renewer must
	// stop (the job is gone, no longer running, or lost a compare race).
	RenewJobLease(ctx context.Context, id int64) (bool, error)
}

// bookkeepingTimeout bounds the post-handler queue bookkeeping run on a context
// that survives the shutdown cancel (context.WithoutCancel), so an in-flight job
// can still be requeued/completed during graceful shutdown without blocking
// teardown indefinitely.
const bookkeepingTimeout = 5 * time.Second

// Handler executes one job's payload. A nil return completes the job; a non-nil
// return requeues it (until the kind's attempt budget is spent). Handlers own
// their own terminal task-row writes (the projection seam); the dispatcher only
// governs the job's queue lifecycle.
type Handler func(ctx context.Context, args []byte) error

// registration binds a handler to its per-kind retry budget.
type registration struct {
	handler     Handler
	maxAttempts int32
}

// Dispatcher polls the job queue and runs claimed jobs on a bounded goroutine
// pool. Handlers may block for a long time (agent polling), so jobs run
// concurrently; the pool cap bounds in-flight work.
type Dispatcher struct {
	src           JobSource
	log           *slog.Logger
	interval      time.Duration
	renewInterval time.Duration
	handlers      map[string]registration
	sem           chan struct{}
	wg            sync.WaitGroup
}

// NewDispatcher constructs a Dispatcher polling src every interval, running at
// most maxConcurrent handlers at once. interval <= 0 defaults to 500ms;
// maxConcurrent <= 0 defaults to 16.
func NewDispatcher(src JobSource, log *slog.Logger, interval time.Duration, maxConcurrent int) *Dispatcher {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 16
	}
	return &Dispatcher{
		src:           src,
		log:           log,
		interval:      interval,
		renewInterval: etcdstore.JobLeaseRenewInterval,
		handlers:      make(map[string]registration),
		sem:           make(chan struct{}, maxConcurrent),
	}
}

// Register binds a job kind to a handler and its retry budget. Not safe to call
// once Run has started.
func (d *Dispatcher) Register(kind string, maxAttempts int32, h Handler) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	d.handlers[kind] = registration{handler: h, maxAttempts: maxAttempts}
}

// Run drains the queue once immediately, then on every tick, until ctx is
// cancelled, after which it waits for in-flight handlers to finish.
func (d *Dispatcher) Run(ctx context.Context) error {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		d.drain(ctx)
		select {
		case <-ctx.Done():
			d.wg.Wait()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// drain claims and dispatches every pending job whose kind is registered, up to
// the concurrency cap. Jobs that cannot get a slot are left for the next drain.
func (d *Dispatcher) drain(ctx context.Context) {
	jobs, err := d.src.PendingJobs(ctx)
	if err != nil {
		d.log.WarnContext(ctx, "worker dispatcher: list pending jobs failed", "error", err)
		return
	}
	for _, j := range jobs {
		reg, ok := d.handlers[j.Kind]
		if !ok {
			continue // not a kind this dispatcher serves
		}
		select {
		case d.sem <- struct{}{}:
		case <-ctx.Done():
			return
		default:
			continue // pool full; pick it up next drain
		}
		claimed, err := d.src.ClaimJob(ctx, j.ID)
		if err != nil || !claimed {
			<-d.sem
			if err != nil {
				d.log.WarnContext(ctx, "worker dispatcher: claim failed", "job_id", j.ID, "error", err)
			}
			continue
		}
		d.wg.Add(1)
		go func(job etcdstore.Job, reg registration) {
			defer d.wg.Done()
			defer func() { <-d.sem }()
			d.execute(ctx, job, reg)
		}(j, reg)
	}
}

// execute runs one claimed job and resolves its queue lifecycle: complete on
// success, requeue (no attempt bump) when graceful shutdown aborted the handler,
// or retry/fail on a real error.
//
// The queue bookkeeping runs on a context derived with context.WithoutCancel so
// it SURVIVES a shutdown cancel of ctx: otherwise an in-flight job aborted by
// SIGTERM would be stranded 'running' forever (PendingJobs is pending-only and
// nothing else transitions running back). The store/etcd stays available until
// Dispatcher.Run returns - cmd/api awaits startWorkers' wg.Wait (which Run's
// in-flight wg.Wait feeds) BEFORE the deferred etcd teardown in serve.go, so
// these writes land. The bg context is bounded by bookkeepingTimeout so teardown
// is not blocked indefinitely.
func (d *Dispatcher) execute(ctx context.Context, j etcdstore.Job, reg registration) {
	done := make(chan struct{})
	defer close(done)
	d.startRenewer(ctx, j.ID, done)

	herr := guardPanic(ctx, d.log, j.Kind, func() error { return reg.handler(ctx, j.Args) })
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	switch {
	case herr != nil && ctx.Err() != nil:
		// Graceful shutdown cancelled the run ctx and the handler aborted on it:
		// requeue the job to pending WITHOUT penalising the attempt counter (a
		// deploy is not a real failure) so the next boot redelivers it.
		if rerr := d.src.RequeueJob(bg, j.ID); rerr != nil {
			d.log.ErrorContext(bg, "worker dispatcher: shutdown requeue failed", "job_id", j.ID, "error", rerr)
		}
	case herr != nil:
		d.log.WarnContext(ctx, "worker job failed", "kind", j.Kind, "job_id", j.ID, "attempts", j.Attempts, "error", herr)
		requeued, rerr := d.src.RetryJob(bg, j.ID, reg.maxAttempts)
		if rerr != nil {
			d.log.ErrorContext(bg, "worker dispatcher: retry bookkeeping failed", "job_id", j.ID, "error", rerr)
			return
		}
		if !requeued {
			d.log.WarnContext(ctx, "worker job exhausted attempts", "kind", j.Kind, "job_id", j.ID)
		}
	default:
		if err := d.src.CompleteJob(bg, j.ID); err != nil {
			d.log.ErrorContext(bg, "worker dispatcher: complete failed", "job_id", j.ID, "error", err)
		}
	}
}

// startRenewer spawns a goroutine that renews job id's lease every renewInterval
// while the handler runs, keeping crash-reclaim time independent of handler
// duration (a long migration never crosses the lease). The renew runs on
// context.WithoutCancel(ctx) so a graceful-shutdown cancel does not stop renewing
// an in-flight job (mirroring the post-handler bookkeeping). The goroutine stops
// when done closes (handler returned) or RenewJobLease reports the lease is no
// longer ours. It is tracked on the WaitGroup so it cannot outlive shutdown.
func (d *Dispatcher) startRenewer(ctx context.Context, id int64, done <-chan struct{}) {
	renewCtx := context.WithoutCancel(ctx)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		t := time.NewTicker(d.renewInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				ok, err := d.src.RenewJobLease(renewCtx, id)
				if err != nil {
					d.log.WarnContext(renewCtx, "worker dispatcher: renew lease failed", "job_id", id, "error", err)
					continue
				}
				if !ok {
					return
				}
			}
		}
	}()
}
