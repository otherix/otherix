// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package console

import (
	"errors"
	"sync"
	"testing"
)

func TestConnectionTracker_AcquireRelease(t *testing.T) {
	t.Parallel()
	c := NewConnectionTracker()

	if err := c.Acquire("vm-1"); err != nil {
		t.Fatalf("Acquire(vm-1) = %v", err)
	}
	if !c.Active("vm-1") {
		t.Error("Active(vm-1) = false after Acquire, want true")
	}

	c.Release("vm-1")
	if c.Active("vm-1") {
		t.Error("Active(vm-1) = true after Release, want false")
	}
}

func TestConnectionTracker_AcquireRejectsConcurrent(t *testing.T) {
	t.Parallel()
	c := NewConnectionTracker()

	if err := c.Acquire("vm-1"); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	err := c.Acquire("vm-1")
	if !errors.Is(err, ErrConsoleInUse) {
		t.Errorf("second Acquire = %v, want ErrConsoleInUse", err)
	}
}

func TestConnectionTracker_AcquirePerVMScoped(t *testing.T) {
	t.Parallel()
	c := NewConnectionTracker()

	if err := c.Acquire("vm-1"); err != nil {
		t.Fatalf("Acquire(vm-1): %v", err)
	}
	if err := c.Acquire("vm-2"); err != nil {
		t.Errorf("Acquire(vm-2) = %v, want nil (different vm)", err)
	}
}

func TestConnectionTracker_ReleaseIdempotent(t *testing.T) {
	t.Parallel()
	c := NewConnectionTracker()

	c.Release("vm-not-acquired")
	c.Release("vm-not-acquired")
	// No panic, no error returned (Release has no return). Pass.

	if err := c.Acquire("vm-1"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	c.Release("vm-1")
	c.Release("vm-1")
	if c.Active("vm-1") {
		t.Error("Active(vm-1) = true after double Release, want false")
	}
}

func TestConnectionTracker_AcquireRejectsEmpty(t *testing.T) {
	t.Parallel()
	c := NewConnectionTracker()
	if err := c.Acquire(""); err == nil {
		t.Error("Acquire(\"\") = nil, want error (empty key would be а stealth shared lock)")
	}
}

func TestConnectionTracker_AcquireConcurrentRace(t *testing.T) {
	t.Parallel()
	c := NewConnectionTracker()

	// Spin 50 goroutines all racing к acquire the same vm. Exactly one
	// should succeed; the remaining 49 should see ErrConsoleInUse.
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		failures  int
	)
	wg.Add(50)
	for range 50 {
		go func() {
			defer wg.Done()
			err := c.Acquire("vm-race")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrConsoleInUse):
				failures++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("successes=%d, want exactly 1", successes)
	}
	if failures != 49 {
		t.Errorf("failures=%d, want 49", failures)
	}
}
