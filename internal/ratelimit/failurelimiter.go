// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ratelimit provides a bounded, in-memory, sliding-window
// failure counter used to throttle repeated authentication failures
// before they reach the expensive argon2id verification path.
//
// The limiter is per-process by design: counters live in memory and are
// not shared across HA replicas. Putting the hot auth path through etcd
// on every attempt would trade a localized DoS concern for a
// cluster-wide one; the accepted consequence is that an attacker who
// spreads requests across N replicas gets up to N times the configured
// budget.
package ratelimit

import (
	"sync"
	"time"
)

// defaultMaxKeys bounds the tracked-key map when Config.MaxKeys is zero
// or negative. The limiter must never be unbounded: it exists to cap a
// resource-exhaustion attack and must not become one itself.
const defaultMaxKeys = 10000

// Config configures a FailureLimiter.
type Config struct {
	// MaxFailures is the number of in-window failures at which a key
	// becomes blocked. Callers that want the limiter disabled must not
	// construct one (use a nil *FailureLimiter) rather than pass a
	// non-positive value here.
	MaxFailures int

	// Window is the sliding window over which failures are counted. A
	// non-positive window means recorded failures expire immediately,
	// so nothing is ever blocked.
	Window time.Duration

	// MaxKeys caps the number of tracked keys. Zero or negative falls
	// back to defaultMaxKeys so memory stays bounded regardless of
	// configuration.
	MaxKeys int

	// now overrides the time source. Nil means time.Now. Unexported:
	// only in-package tests inject it to drive window expiry without
	// sleeping.
	now func() time.Time
}

// entry holds the in-window failure timestamps for one key, ordered
// oldest first.
type entry struct {
	times []time.Time
}

// FailureLimiter is a thread-safe, sliding-window counter of failures
// keyed by an opaque string (e.g. "ip:..." or "email:..."). All methods
// are safe for concurrent use; a single mutex guards the key map. There
// is no background goroutine: expired state is pruned opportunistically
// on every read and write.
//
// Memory is bounded by Config.MaxKeys. When a failure arrives for a new
// key at capacity, fully-expired keys are swept first; if the map is
// still full, the key whose NEWEST timestamp is oldest (the one closest
// to expiring on its own) is evicted. Accepted weakness: a flood of
// distinct keys can evict a real attacker's nearly-expired entry early.
// That is acceptable because the attacker still paid the in-window cost
// to get there, and the per-email key independently constrains attacks
// against a single account.
type FailureLimiter struct {
	cfg  Config
	mu   sync.Mutex
	keys map[string]*entry
}

// New constructs a FailureLimiter from cfg. A nil cfg.now defaults to
// time.Now; a non-positive cfg.MaxKeys defaults to defaultMaxKeys.
func New(cfg Config) *FailureLimiter {
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = defaultMaxKeys
	}
	return &FailureLimiter{cfg: cfg, keys: make(map[string]*entry)}
}

// Blocked reports whether key has accumulated at least MaxFailures
// failures within the last Window. When blocked, retryAfter is the time
// remaining until the oldest in-window failure ages out, i.e. the
// earliest moment the key can become unblocked; callers use it to set a
// Retry-After header. Expired timestamps for the key are pruned as a
// side effect.
func (l *FailureLimiter) Blocked(key string) (blocked bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.cfg.now()
	e, ok := l.keys[key]
	if !ok {
		return false, 0
	}
	e.prune(now.Add(-l.cfg.Window))
	if len(e.times) == 0 {
		delete(l.keys, key)
		return false, 0
	}
	if len(e.times) < l.cfg.MaxFailures {
		return false, 0
	}
	return true, e.times[0].Add(l.cfg.Window).Sub(now)
}

// RecordFailure appends a failure timestamp for key, pruning the key's
// expired timestamps first. When key is new and the map is at MaxKeys
// capacity, fully-expired keys are swept and, if the map is still full,
// the key closest to expiring is evicted (see the type doc for the
// policy and its accepted weakness).
func (l *FailureLimiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.cfg.now()
	cutoff := now.Add(-l.cfg.Window)

	if e, ok := l.keys[key]; ok {
		e.prune(cutoff)
		e.times = append(e.times, now)
		return
	}
	if len(l.keys) >= l.cfg.MaxKeys {
		l.evictLocked(cutoff)
	}
	l.keys[key] = &entry{times: []time.Time{now}}
}

// evictLocked makes room for one new key: it drops every key whose
// newest timestamp is fully outside the window, then, if the map is
// still at capacity, drops the single key whose newest timestamp is
// oldest. Callers must hold l.mu.
func (l *FailureLimiter) evictLocked(cutoff time.Time) {
	for k, e := range l.keys {
		if len(e.times) == 0 || !e.times[len(e.times)-1].After(cutoff) {
			delete(l.keys, k)
		}
	}
	if len(l.keys) < l.cfg.MaxKeys {
		return
	}
	var (
		victim       string
		victimNewest time.Time
		found        bool
	)
	for k, e := range l.keys {
		newest := e.times[len(e.times)-1]
		if !found || newest.Before(victimNewest) {
			victim, victimNewest, found = k, newest, true
		}
	}
	if found {
		delete(l.keys, victim)
	}
}

// prune drops timestamps at or before cutoff. Timestamps are appended
// in monotonically non-decreasing order (every write stamps now under
// the same mutex), so a single scan from the front suffices.
func (e *entry) prune(cutoff time.Time) {
	i := 0
	for i < len(e.times) && !e.times[i].After(cutoff) {
		i++
	}
	if i > 0 {
		e.times = append(e.times[:0], e.times[i:]...)
	}
}
