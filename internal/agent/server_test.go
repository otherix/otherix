// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunSupervisedRestartsOnError verifies the supervisor re-invokes run
// after a non-cancel error: a run that fails N times then blocks until ctx
// cancel must be called exactly N+1 times. This is the core teeth - it fails
// if run is invoked only once (the unsupervised behaviour that left DNS dark
// after a transient bind failure).
func TestRunSupervisedRestartsOnError(t *testing.T) {
	const failures = 3
	var calls int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := func(ctx context.Context) error {
		n := atomic.AddInt32(&calls, 1)
		if n <= failures {
			return errors.New("transient bind failure")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	done := runSupervised(ctx, "test", run, 1*time.Millisecond, 5*time.Millisecond, discardLogger())

	// Poll until the success call (failures+1) lands, bounded.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) < failures+1 {
		if time.Now().After(deadline) {
			t.Fatalf("run called %d times, want %d within deadline", atomic.LoadInt32(&calls), failures+1)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("done not closed after ctx cancel")
	}

	if got := atomic.LoadInt32(&calls); got != failures+1 {
		t.Errorf("run call count = %d, want %d", got, failures+1)
	}
}

// TestRunSupervisedCleanCancelNoRestart verifies a clean ctx-cancel return
// does not trigger a restart: run is invoked exactly once and done closes
// promptly.
func TestRunSupervisedCleanCancelNoRestart(t *testing.T) {
	var calls int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		<-ctx.Done()
		return ctx.Err()
	}

	done := runSupervised(ctx, "test", run, 1*time.Millisecond, 5*time.Millisecond, discardLogger())

	// Let the goroutine enter run.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&calls) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("run not invoked")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("done not closed after ctx cancel")
	}

	// Give any erroneous restart a moment to surface.
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("run call count = %d, want 1 (clean cancel must not restart)", got)
	}
}

// TestRunSupervisedCancelDuringBackoff verifies the backoff wait selects on
// ctx.Done(): cancelling ctx while the supervisor is sleeping between restarts
// closes done promptly, well under maxBackoff.
func TestRunSupervisedCancelDuringBackoff(t *testing.T) {
	var calls int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := func(ctx context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("always fails, forcing a backoff wait")
	}

	// Large maxBackoff so a blind time.Sleep would not return promptly.
	done := runSupervised(ctx, "test", run, 10*time.Second, 30*time.Second, discardLogger())

	// Wait until run has failed at least once and we are in the backoff wait.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&calls) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("run not invoked")
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("done not closed during backoff wait; wait does not select on ctx.Done()")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("done closed after %v, want prompt return well under maxBackoff", elapsed)
	}
}

// TestAwaitDoneAllClosed verifies the bounded drain returns promptly with
// timedOut=false when every done-channel is already closed.
func TestAwaitDoneAllClosed(t *testing.T) {
	c1 := make(chan struct{})
	c2 := make(chan struct{})
	close(c1)
	close(c2)

	dones := []namedDone{
		{name: "a", done: c1},
		{name: "b", done: c2},
	}

	start := time.Now()
	timedOut, stuck := awaitDone(dones, 50*time.Millisecond)
	elapsed := time.Since(start)

	if timedOut {
		t.Errorf("awaitDone(all-closed) timedOut = true, want false")
	}
	if len(stuck) != 0 {
		t.Errorf("awaitDone(all-closed) stuck = %v, want empty", stuck)
	}
	if elapsed > time.Second {
		t.Errorf("awaitDone(all-closed) took %v, want prompt return well under the timeout", elapsed)
	}
}

// TestAwaitDoneOneStuck verifies the bound: when one done-channel never
// closes, awaitDone returns timedOut=true after roughly the timeout and
// names the stuck channel, rather than blocking forever.
//
// Revert-to-confirm: if the shutdown wait were an unconditional
// `<-done` (the pre-fix code), this test would HANG on the never-closed
// channel and the test binary would be killed by the go-test timeout.
// The helper's bound is precisely what lets it return; this test has
// teeth because it would not terminate without the bound.
func TestAwaitDoneOneStuck(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	never := make(chan struct{}) // intentionally never closed

	dones := []namedDone{
		{name: "fast", done: closed},
		{name: "wedged", done: never},
	}

	start := time.Now()
	timedOut, stuck := awaitDone(dones, 50*time.Millisecond)
	elapsed := time.Since(start)

	if !timedOut {
		t.Errorf("awaitDone(one-stuck) timedOut = false, want true")
	}
	if len(stuck) != 1 || stuck[0] != "wedged" {
		t.Errorf("awaitDone(one-stuck) stuck = %v, want [wedged]", stuck)
	}
	// Tolerant ceiling to avoid CI flakiness: assert it returned and did
	// not hang, not tight timing.
	if elapsed > 5*time.Second {
		t.Errorf("awaitDone(one-stuck) took %v, want bounded return near the timeout", elapsed)
	}
}
