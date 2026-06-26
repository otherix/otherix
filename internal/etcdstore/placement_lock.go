// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"sync"
)

// placementLocker is the process-local backend for Store.AcquirePlacementLock on
// the single-control-plane default. It serializes the read-availability ->
// pin-commit window of one placement against every other placement, so
// concurrent placements observe each other's pin before scoring and the
// LeastAllocated scorer spreads VMs across nodes instead of co-locating. It is
// keyed by the store.LockKey* namespace; each key is an independent capacity-1
// semaphore. The HA path replaces this backend with an etcd lock keyed by the
// same lockKey, leaving the AcquirePlacementLock contract unchanged.
type placementLocker struct {
	mu   sync.Mutex
	sems map[int64]chan struct{}
}

func newPlacementLocker() *placementLocker {
	return &placementLocker{sems: make(map[int64]chan struct{})}
}

// sem returns the capacity-1 semaphore for key, creating it on first use.
func (l *placementLocker) sem(key int64) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch, ok := l.sems[key]
	if !ok {
		ch = make(chan struct{}, 1)
		l.sems[key] = ch
	}
	return ch
}

// acquire blocks until the lock for key is held or ctx is cancelled. It returns a
// release func the caller MUST defer. On a ctx error the returned func is a safe
// no-op, so a blind defer release() after the error check never double-frees. The
// real release is idempotent (sync.Once) so an accidental second call is a no-op
// rather than a deadlock-by-underflow.
func (l *placementLocker) acquire(ctx context.Context, key int64) (release func(), err error) {
	ch := l.sem(key)
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}
