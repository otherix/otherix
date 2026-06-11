// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced time source so window-expiry tests
// run deterministically without sleeping.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)}
}

func TestBlocked_UnderThreshold(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{MaxFailures: 3, Window: 15 * time.Minute, MaxKeys: 100, now: clock.Now})

	l.RecordFailure("ip:192.0.2.1")
	l.RecordFailure("ip:192.0.2.1")

	blocked, _ := l.Blocked("ip:192.0.2.1")
	if blocked {
		t.Errorf("Blocked(key) after 2 failures with MaxFailures=3 = true, want false")
	}
}

func TestBlocked_AtThreshold(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{MaxFailures: 3, Window: 15 * time.Minute, MaxKeys: 100, now: clock.Now})

	for i := 0; i < 3; i++ {
		l.RecordFailure("ip:192.0.2.1")
	}

	blocked, retryAfter := l.Blocked("ip:192.0.2.1")
	if !blocked {
		t.Errorf("Blocked(key) after 3 failures with MaxFailures=3 = false, want true")
	}
	if retryAfter <= 0 {
		t.Errorf("Blocked(key) retryAfter = %v, want > 0", retryAfter)
	}
}

func TestBlocked_UnblocksAfterWindow(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{MaxFailures: 3, Window: 15 * time.Minute, MaxKeys: 100, now: clock.Now})

	for i := 0; i < 3; i++ {
		l.RecordFailure("ip:192.0.2.1")
	}
	if blocked, _ := l.Blocked("ip:192.0.2.1"); !blocked {
		t.Fatalf("Blocked(key) at threshold = false, want true")
	}

	clock.Advance(15*time.Minute + time.Second)

	blocked, _ := l.Blocked("ip:192.0.2.1")
	if blocked {
		t.Errorf("Blocked(key) after window elapsed = true, want false (timestamps aged out)")
	}
}

func TestBlocked_RetryAfterEqualsOldestExpiry(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{MaxFailures: 3, Window: 10 * time.Minute, MaxKeys: 100, now: clock.Now})

	// Failures at t0, t0+1m, t0+2m. At t0+5m the oldest in-window
	// failure (t0) ages out at t0+10m, so retryAfter must be 5m.
	l.RecordFailure("k")
	clock.Advance(time.Minute)
	l.RecordFailure("k")
	clock.Advance(time.Minute)
	l.RecordFailure("k")
	clock.Advance(3 * time.Minute)

	blocked, retryAfter := l.Blocked("k")
	if !blocked {
		t.Fatalf("Blocked(k) = false, want true")
	}
	if want := 5 * time.Minute; retryAfter != want {
		t.Errorf("Blocked(k) retryAfter = %v, want %v", retryAfter, want)
	}

	// After the oldest failure ages out, only 2 of 3 remain in-window:
	// no longer blocked.
	clock.Advance(5*time.Minute + time.Second)
	if blocked, _ := l.Blocked("k"); blocked {
		t.Errorf("Blocked(k) after oldest failure aged out = true, want false")
	}
}

func TestRecordFailure_EvictsOldestNewestAtCapacity(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{MaxFailures: 1, Window: 15 * time.Minute, MaxKeys: 2, now: clock.Now})

	// Three distinct keys at one-minute spacing. At capacity, inserting
	// "c" must evict "a" (its newest timestamp is the oldest).
	l.RecordFailure("a")
	clock.Advance(time.Minute)
	l.RecordFailure("b")
	clock.Advance(time.Minute)
	l.RecordFailure("c")

	if got := len(l.keys); got > 2 {
		t.Errorf("len(keys) after 3 distinct keys with MaxKeys=2 = %d, want <= 2", got)
	}
	if blocked, _ := l.Blocked("a"); blocked {
		t.Errorf("Blocked(a) = true, want false (a should have been evicted)")
	}
	if blocked, _ := l.Blocked("b"); !blocked {
		t.Errorf("Blocked(b) = false, want true (b should survive eviction)")
	}
	if blocked, _ := l.Blocked("c"); !blocked {
		t.Errorf("Blocked(c) = false, want true (c was just recorded)")
	}
}

func TestRecordFailure_SweepsExpiredBeforeEvicting(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{MaxFailures: 1, Window: 10 * time.Minute, MaxKeys: 2, now: clock.Now})

	// "a" fully expires before "c" is inserted, so the sweep drops "a"
	// and the live "b" entry must NOT be evicted.
	l.RecordFailure("a")
	clock.Advance(11 * time.Minute)
	l.RecordFailure("b")
	clock.Advance(time.Minute)
	l.RecordFailure("c")

	if got := len(l.keys); got > 2 {
		t.Errorf("len(keys) = %d, want <= 2", got)
	}
	if blocked, _ := l.Blocked("b"); !blocked {
		t.Errorf("Blocked(b) = false, want true (expired a should be swept instead of evicting live b)")
	}
	if blocked, _ := l.Blocked("c"); !blocked {
		t.Errorf("Blocked(c) = false, want true")
	}
}

func TestFailureLimiter_ConcurrentUse(t *testing.T) {
	// Real clock here: the test asserts only race-freedom and bounded
	// growth, not window timing, so no sleeps are needed.
	l := New(Config{MaxFailures: 5, Window: time.Minute, MaxKeys: 8})

	t.Run("hammer", func(t *testing.T) {
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				key := fmt.Sprintf("k%d", g%10)
				for i := 0; i < 200; i++ {
					l.RecordFailure(key)
					l.Blocked(key)
				}
			}(g)
		}
		wg.Wait()
	})

	l.mu.Lock()
	defer l.mu.Unlock()
	if got := len(l.keys); got > 8 {
		t.Errorf("len(keys) after concurrent hammer = %d, want <= MaxKeys (8)", got)
	}
}
