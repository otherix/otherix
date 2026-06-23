// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dnsproxy

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// newRateForwarder builds a Forwarder wired only for rate-limit tests: a fixed
// per-source rate, a small source-map cap, and a controllable clock.
func newRateForwarder(t *testing.T, qps int, mapCap int, clock func() time.Time) *Forwarder {
	t.Helper()
	f, err := New(Config{
		Listen:                       "127.0.0.1:0",
		Upstreams:                    []string{"192.0.2.1:53"},
		Log:                          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxQueriesPerSecondPerSource: qps,
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	if mapCap > 0 {
		f.maxRateLimitedSources = mapCap
	}
	if clock != nil {
		f.now = clock
	}
	return f
}

// TestTokenBucketAllowDenyRefill: a fresh bucket allows up to capacity queries
// at one instant, denies the next, then allows one more after the clock advances
// enough to refill a single token.
func TestTokenBucketAllowDenyRefill(t *testing.T) {
	t.Parallel()
	const rate = 5.0
	now := time.Unix(1000, 0)
	b := newTokenBucket(now, rate)
	// Full bucket: capacity == rate allowances at the same instant.
	for i := 0; i < int(rate); i++ {
		if got := b.allow(now, rate); !got {
			t.Errorf("allow #%d at full bucket = false, want true", i+1)
		}
	}
	// Exhausted: the next query at the same instant is denied.
	if got := b.allow(now, rate); got {
		t.Errorf("allow at exhausted bucket = true, want false")
	}
	// Advance the clock by 1/rate seconds: exactly one token refills.
	refillOne := now.Add(time.Duration(float64(time.Second) / rate))
	if got := b.allow(refillOne, rate); !got {
		t.Errorf("allow after one-token refill = false, want true")
	}
	// And that single token is spent again.
	if got := b.allow(refillOne, rate); got {
		t.Errorf("allow after spending refilled token = true, want false")
	}
}

// TestRateAllowPerSourceIsolation: one source exhausting its bucket does not
// affect a different source at the same instant.
func TestRateAllowPerSourceIsolation(t *testing.T) {
	t.Parallel()
	const rate = 3
	now := time.Unix(2000, 0)
	f := newRateForwarder(t, rate, 0, func() time.Time { return now })
	for i := 0; i < rate; i++ {
		if got := f.rateAllow("10.0.0.1", now); !got {
			t.Errorf("rateAllow(A) #%d = false, want true", i+1)
		}
	}
	if got := f.rateAllow("10.0.0.1", now); got {
		t.Errorf("rateAllow(A) after exhaustion = true, want false")
	}
	// Source B is untouched and gets its full allowance.
	if got := f.rateAllow("10.0.0.2", now); !got {
		t.Errorf("rateAllow(B) = false, want true")
	}
}

// TestRateAllowBoundedMapFailOpen: at the source-map cap with every tracked
// source actively rate-limited, a brand-new source is allowed (fail open) and
// the map never exceeds the cap.
func TestRateAllowBoundedMapFailOpen(t *testing.T) {
	t.Parallel()
	const (
		rate   = 2
		mapCap = 4
	)
	now := time.Unix(3000, 0)
	f := newRateForwarder(t, rate, mapCap, func() time.Time { return now })
	// Fill the map to cap with sources that are actively rate-limited (each
	// exhausts its bucket at the same instant, so none is idle/full).
	for i := 0; i < mapCap; i++ {
		ip := sourceLabel(i)
		for q := 0; q < rate; q++ {
			f.rateAllow(ip, now)
		}
		if got := f.rateAllow(ip, now); got {
			t.Errorf("rateAllow(%q) after exhaustion = true, want false", ip)
		}
	}
	if got := f.rateMapLen(); got != mapCap {
		t.Fatalf("map length after filling = %d, want %d", got, mapCap)
	}
	// A brand-new source at the same instant: no idle bucket to evict, so fail open.
	if got := f.rateAllow("new-source", now); !got {
		t.Errorf("rateAllow(new-source) at full map of busy sources = false, want true (fail open)")
	}
	if got := f.rateMapLen(); got > mapCap {
		t.Errorf("map length = %d, want <= %d (must not grow past cap)", got, mapCap)
	}
}

// TestRateAllowIdleBucketEviction: at cap, advancing the clock until one tracked
// source's bucket refills to full (idle) lets a new source reuse that slot; the
// map stays at cap and the idle source is evicted.
func TestRateAllowIdleBucketEviction(t *testing.T) {
	t.Parallel()
	const (
		rate   = 2
		mapCap = 3
	)
	start := time.Unix(4000, 0)
	now := start
	f := newRateForwarder(t, rate, mapCap, func() time.Time { return now })
	// Fill the map to cap, exhausting each source's bucket.
	for i := 0; i < mapCap; i++ {
		ip := sourceLabel(i)
		for q := 0; q <= rate; q++ {
			f.rateAllow(ip, now)
		}
	}
	if got := f.rateMapLen(); got != mapCap {
		t.Fatalf("map length after filling = %d, want %d", got, mapCap)
	}
	// Advance well past the refill window so every tracked bucket is idle/full.
	now = start.Add(10 * time.Second)
	// A new source reuses an evicted idle slot; the map stays at cap.
	if got := f.rateAllow("new-source", now); !got {
		t.Errorf("rateAllow(new-source) after idle window = false, want true")
	}
	if got := f.rateMapLen(); got != mapCap {
		t.Errorf("map length after eviction+insert = %d, want %d (stay at cap)", got, mapCap)
	}
	if !f.rateMapHas("new-source") {
		t.Errorf("new-source not tracked after eviction; want it inserted into the freed slot")
	}
}

// TestRateLimitedAccessorIncrements: RateLimited counts denied queries, mirroring
// the SourceDropped accessor.
func TestRateLimitedAccessorIncrements(t *testing.T) {
	t.Parallel()
	const rate = 1
	now := time.Unix(5000, 0)
	f := newRateForwarder(t, rate, 0, func() time.Time { return now })
	if !f.rateAllow("10.0.0.9", now) {
		t.Fatalf("first rateAllow = false, want true")
	}
	if f.rateAllow("10.0.0.9", now) {
		t.Fatalf("second rateAllow = true, want false")
	}
	if got := f.RateLimited(); got != 0 {
		t.Errorf("RateLimited after a bare rateAllow deny = %d, want 0 (Run accounts the drop)", got)
	}
}

// sourceLabel returns a distinct synthetic source key for index i.
func sourceLabel(i int) string {
	return "src-" + time.Duration(i).String() + "-" + string(rune('a'+i%26))
}

// rateMapLen returns the number of tracked source buckets.
func (f *Forwarder) rateMapLen() int {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return len(f.buckets)
}

// rateMapHas reports whether ip currently has a tracked bucket.
func (f *Forwarder) rateMapHas(ip string) bool {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	_, ok := f.buckets[ip]
	return ok
}
