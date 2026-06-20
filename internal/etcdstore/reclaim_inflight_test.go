//go:build integration

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTryBeginReclaim(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	digest := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	node := uuid.New()

	began, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim first = %v, want nil", err)
	}
	if !began {
		t.Errorf("TryBeginReclaim first = false, want true (fresh marker)")
	}

	began2, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim second = %v, want nil", err)
	}
	if began2 {
		t.Errorf("TryBeginReclaim second = true, want false (marker still fresh)")
	}

	if err := st.EndReclaim(ctx, digest, node); err != nil {
		t.Fatalf("EndReclaim = %v, want nil", err)
	}

	began3, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim after end = %v, want nil", err)
	}
	if !began3 {
		t.Errorf("TryBeginReclaim after EndReclaim = false, want true (marker cleared)")
	}
}

func TestTryBeginReclaimExpired(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	digest := "00f100f100f100f100f100f100f100f100f100f100f100f100f100f100f100f1"
	node := uuid.New()

	began, err := st.TryBeginReclaim(ctx, digest, node, -time.Second)
	if err != nil {
		t.Fatalf("TryBeginReclaim = %v, want nil", err)
	}
	if !began {
		t.Errorf("TryBeginReclaim = false, want true")
	}
	began2, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim over expired = %v, want nil", err)
	}
	if !began2 {
		t.Errorf("TryBeginReclaim over expired marker = false, want true")
	}
}

// TestTryBeginReclaimConcurrent drives the seam the atomic create-if-absent
// protects: two replicas racing the first reclaim of the same (digest, node)
// must yield exactly one winner, never two enqueues.
func TestTryBeginReclaimConcurrent(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	digest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc1"
	node := uuid.New()

	const racers = 8
	var wg sync.WaitGroup
	results := make([]bool, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.TryBeginReclaim(ctx, digest, node, time.Minute)
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d TryBeginReclaim = %v, want nil", i, errs[i])
		}
		if results[i] {
			won++
		}
	}
	if won != 1 {
		t.Errorf("concurrent TryBeginReclaim winners = %d, want exactly 1", won)
	}
}
