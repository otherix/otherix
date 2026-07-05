// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// PeriodicFunc is one tick of periodic maintenance. A non-nil return is logged
// and the schedule continues; periodic work is retry-on-next-tick.
type PeriodicFunc func(ctx context.Context) error

// periodicJob binds a periodic function to its cadence.
type periodicJob struct {
	name       string
	interval   time.Duration
	runOnStart bool
	fn         PeriodicFunc
}

// Scheduler runs registered periodic functions on independent tickers, the
// etcd-runtime periodic scheduler. It drives the
// node-health reconcile, the scan trigger, and the retention sweeps; unlike the
// dispatcher it does not consume the job queue.
type Scheduler struct {
	log  *slog.Logger
	jobs []periodicJob
}

// NewScheduler constructs an empty Scheduler.
func NewScheduler(log *slog.Logger) *Scheduler {
	return &Scheduler{log: log}
}

// Register adds a periodic function firing every interval. runOnStart fires it
// once immediately on Run (e.g. heartbeat reconcile, so a restart promotes nodes
// that kept heartbeating without waiting a full tick). Not safe to call once Run
// has started.
func (s *Scheduler) Register(name string, interval time.Duration, runOnStart bool, fn PeriodicFunc) {
	s.jobs = append(s.jobs, periodicJob{name: name, interval: interval, runOnStart: runOnStart, fn: fn})
}

// Run launches one goroutine per registered job and blocks until ctx is
// cancelled, then waits for in-flight ticks to finish.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, j := range s.jobs {
		wg.Add(1)
		go func(j periodicJob) {
			defer wg.Done()
			s.loop(ctx, j)
		}(j)
	}
	wg.Wait()
	return ctx.Err()
}

// loop fires a job on start (when requested) and then on every tick.
func (s *Scheduler) loop(ctx context.Context, j periodicJob) {
	if j.runOnStart {
		s.fire(ctx, j)
	}
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.fire(ctx, j)
		}
	}
}

// fire runs one tick, logging a non-nil result at warn.
func (s *Scheduler) fire(ctx context.Context, j periodicJob) {
	if err := guardPanic(ctx, s.log, j.name, func() error { return j.fn(ctx) }); err != nil {
		s.log.WarnContext(ctx, "periodic job failed", "job", j.name, "error", err)
	}
}
