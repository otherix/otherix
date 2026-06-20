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

func TestReplicateInflightMarker(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	node := uuid.New()

	ok, err := s.TryBeginReplicate(ctx, digest, node, time.Minute)
	if err != nil || !ok {
		t.Fatalf("TryBeginReplicate first = %v, %v; want true, nil", ok, err)
	}
	ok, err = s.TryBeginReplicate(ctx, digest, node, time.Minute)
	if err != nil || ok {
		t.Fatalf("TryBeginReplicate while in-flight = %v, %v; want false, nil", ok, err)
	}
	if err := s.EndReplicate(ctx, digest, node); err != nil {
		t.Fatalf("EndReplicate: %v", err)
	}
	ok, err = s.TryBeginReplicate(ctx, digest, node, time.Minute)
	if err != nil || !ok {
		t.Fatalf("TryBeginReplicate after end = %v, %v; want true, nil", ok, err)
	}
	// A marker created with a negative ttl is already expired -> the next begin reclaims it.
	s.EndReplicate(ctx, digest, node)
	if ok, _ := s.TryBeginReplicate(ctx, digest, node, -time.Second); !ok {
		t.Fatalf("TryBeginReplicate with negative ttl should still succeed (writes an already-expired marker)")
	}
	if ok, _ := s.TryBeginReplicate(ctx, digest, node, time.Minute); !ok {
		t.Fatalf("TryBeginReplicate did not reclaim an expired marker")
	}
	s.EndReplicate(ctx, digest, node)
}

// TestTryBeginReplicateConcurrent drives the seam the atomic create-if-absent
// protects: two replicas racing the first replication of the same (digest, node)
// must yield exactly one winner, never two enqueues.
func TestTryBeginReplicateConcurrent(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	digest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd1"
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
			results[i], errs[i] = s.TryBeginReplicate(ctx, digest, node, time.Minute)
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i := range racers {
		if errs[i] != nil {
			t.Fatalf("racer %d TryBeginReplicate = %v, want nil", i, errs[i])
		}
		if results[i] {
			won++
		}
	}
	if won != 1 {
		t.Errorf("concurrent TryBeginReplicate winners = %d, want exactly 1", won)
	}
}
