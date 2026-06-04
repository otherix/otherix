// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"testing"
	"time"
)

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
