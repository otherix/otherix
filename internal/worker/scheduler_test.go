// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerRunsOnStartAndTicks(t *testing.T) {
	var calls atomic.Int32
	s := NewScheduler(discardLogger())
	s.Register("tick", 5*time.Millisecond, true, func(context.Context) error {
		calls.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	// run-on-start fires once, then ticks accumulate.
	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("scheduler fired %d times, want >= 3", calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestSchedulerErrorDoesNotStopSchedule(t *testing.T) {
	var calls atomic.Int32
	s := NewScheduler(discardLogger())
	s.Register("flaky", 3*time.Millisecond, true, func(context.Context) error {
		calls.Add(1)
		return context.DeadlineExceeded // always errors
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("flaky job fired %d times despite errors, want >= 3", calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestFireRecoversPanic ensures a panic in a periodic func is recovered rather
// than propagated: a panic in a scheduler goroutine crashes the whole process
// (embedded etcd with it). fire must convert it to a logged error and return.
func TestFireRecoversPanic(t *testing.T) {
	s := NewScheduler(discardLogger())
	j := periodicJob{name: "poison", fn: func(context.Context) error { panic("boom") }}
	// Reaching the line after fire = the panic was recovered, not propagated.
	s.fire(context.Background(), j)
}
