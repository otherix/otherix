// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPlacementLockerMutualExclusion proves the locker serializes contending
// acquirers of one key: with eight goroutines racing into the critical section
// the maximum observed concurrent holder count is exactly 1. Revert-to-confirm:
// make acquire return a no-op release without taking the semaphore and this
// observes 2+.
func TestPlacementLockerMutualExclusion(t *testing.T) {
	l := newPlacementLocker()
	const key = 1
	var inside, maxSeen atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // barrier: all contend at once
			release, err := l.acquire(context.Background(), key)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m || maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond) // widen the window a real overlap would exploit
			inside.Add(-1)
			release()
		}()
	}
	close(start)
	wg.Wait()
	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent holders = %d, want 1 (mutual exclusion)", got)
	}
}

// TestPlacementLockerCtxCancelWhileWaiting proves a waiter whose ctx is cancelled
// returns ctx.Err() without acquiring, and does not corrupt the semaphore (a
// later acquire after the holder releases still works).
func TestPlacementLockerCtxCancelWhileWaiting(t *testing.T) {
	l := newPlacementLocker()
	const key = 1
	hold, err := l.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := l.acquire(ctx, key)
	if err == nil {
		release()
		t.Fatalf("acquire on cancelled ctx = nil err, want ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	hold() // releasing the holder must leave the semaphore usable
	again, err := l.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	again()
}

// TestPlacementLockerDistinctKeysDoNotBlock proves keys are independent: holding
// key 1 never blocks an acquire of key 2.
func TestPlacementLockerDistinctKeysDoNotBlock(t *testing.T) {
	l := newPlacementLocker()
	r1, err := l.acquire(context.Background(), 1)
	if err != nil {
		t.Fatalf("acquire key 1: %v", err)
	}
	defer r1()
	done := make(chan error, 1)
	go func() {
		r2, aerr := l.acquire(context.Background(), 2)
		if aerr == nil {
			r2()
		}
		done <- aerr
	}()
	select {
	case aerr := <-done:
		if aerr != nil {
			t.Errorf("acquire key 2 while key 1 held: %v", aerr)
		}
	case <-time.After(time.Second):
		t.Fatal("acquire on a distinct key blocked behind key 1")
	}
}
