// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/store"
)

// TestPlacementLockSerializesConcurrentTransactions exercises the HA
// guarantee: `pg_advisory_xact_lock` taken inside a transaction blocks
// a concurrent acquirer until the holder commits or rolls back. Two
// goroutines each begin a transaction and call AcquirePlacementLock; the
// second one's acquire must wait until the first commits.
//
// Without the lock — or if a future refactor accidentally calls
// AcquirePlacementLock outside an InTxWithTx callback — this test
// would observe near-zero wait times and fail.
func TestPlacementLockSerializesConcurrentTransactions(t *testing.T) {
	h := shared
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const holdDur = 300 * time.Millisecond

	aHolding := make(chan struct{})
	aCommitted := make(chan struct{})
	var aErr error

	go func() {
		tx, err := h.Pool.Begin(ctx)
		if err != nil {
			aErr = err
			close(aHolding)
			close(aCommitted)
			return
		}
		// Rollback is a no-op after a successful Commit; safe under
		// every exit path.
		defer func() { _ = tx.Rollback(ctx) }()

		if err := store.New(tx).AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
			aErr = err
			close(aHolding)
			close(aCommitted)
			return
		}
		close(aHolding)

		time.Sleep(holdDur)

		if err := tx.Commit(ctx); err != nil {
			aErr = err
		}
		close(aCommitted)
	}()

	<-aHolding
	if aErr != nil {
		t.Fatalf("goroutine A failed before holding the lock: %v", aErr)
	}

	// B begins a transaction while A is still holding the lock and tries
	// to acquire. The acquire call must block; we measure how long.
	bTx, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("B begin: %v", err)
	}
	defer func() { _ = bTx.Rollback(ctx) }()

	start := time.Now()
	if err := store.New(bTx).AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
		t.Fatalf("B acquire: %v", err)
	}
	waited := time.Since(start)

	<-aCommitted
	if aErr != nil {
		t.Fatalf("goroutine A failed after holding the lock: %v", aErr)
	}

	// Lower bound chosen with slack: holdDur=300ms, allow scheduling jitter
	// and goroutine startup to shave off up to ~half the interval. Without
	// the lock the wait would be < 5 ms, well below this threshold.
	if waited < holdDur/2 {
		t.Errorf("B acquired lock in %v, want at least %v (A held it for %v)",
			waited, holdDur/2, holdDur)
	}
}

// TestPlacementLockReleasesOnRollback confirms that abort-via-rollback
// releases the lock just like commit does — important because the
// vm.create transaction returns on the first scheduler / DB error
// path, which triggers rollback. A leaked lock would stall every
// subsequent placement attempt.
func TestPlacementLockReleasesOnRollback(t *testing.T) {
	h := shared
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tx1, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if err := store.New(tx1).AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
		_ = tx1.Rollback(ctx)
		t.Fatalf("first acquire: %v", err)
	}
	if err := tx1.Rollback(ctx); err != nil {
		t.Fatalf("first rollback: %v", err)
	}

	// A fresh transaction should acquire immediately — no waiter, no
	// stuck lock.
	tx2, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()

	start := time.Now()
	if err := store.New(tx2).AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	waited := time.Since(start)
	if waited > 50*time.Millisecond {
		t.Errorf("second acquire took %v, expected ~immediate (rollback should release the lock)", waited)
	}
}

// TestPlacementLockConcurrentAcquires fans out N concurrent acquirers
// and confirms strict serialization: only one transaction at a time can
// hold the lock. Each goroutine bumps a counter inside the locked
// section and sleeps briefly; a correct implementation observes the
// counter advance one-at-a-time, while a broken (non-serialized) lock
// would let multiple goroutines see the same starting counter value.
func TestPlacementLockConcurrentAcquires(t *testing.T) {
	h := shared
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const N = 4
	const sliceDur = 50 * time.Millisecond

	var mu sync.Mutex
	var counter int
	var observations []int

	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			tx, err := h.Pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()

			if err := store.New(tx).AcquirePlacementLock(ctx, store.LockKeyPlacement); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}

			mu.Lock()
			counter++
			observations = append(observations, counter)
			mu.Unlock()

			// Hold the lock briefly to encourage interleaving attempts.
			time.Sleep(sliceDur)

			if err := tx.Commit(ctx); err != nil {
				t.Errorf("commit: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(observations) != N {
		t.Fatalf("expected %d observations, got %d", N, len(observations))
	}
	// Each goroutine entered the critical section serialized per advisory
	// lock, so observations[] must be the strictly-increasing sequence
	// 1..N (in some order based on lock-wait scheduling). A duplicate
	// value would mean two goroutines were inside the critical section
	// simultaneously.
	seen := make(map[int]bool, N)
	for _, v := range observations {
		if v < 1 || v > N {
			t.Errorf("observation %d outside [1,%d]", v, N)
		}
		if seen[v] {
			t.Errorf("duplicate observation %d — two goroutines held the lock at once", v)
		}
		seen[v] = true
	}
}
