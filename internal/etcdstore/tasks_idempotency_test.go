// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// idemParams returns a create-task params carrying a full idempotency
// descriptor for user u, key, and request hash.
func idemParams(u uuid.UUID, key string, hash []byte) store.CreateTaskParams {
	p := taskParams(store.TaskStatusPending, &u)
	p.IdempotencyUserID = &u
	p.IdempotencyKey = &key
	p.IdempotencyHash = hash
	return p
}

// countTasks returns the number of task rows in the store.
func countTasks(t *testing.T, s *etcdstore.Store) int {
	t.Helper()
	all, err := s.ListTasksAny(context.Background(), store.ListTasksAnyParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	return len(all)
}

// countJobs returns the number of job rows in the store's queue.
func countJobs(t *testing.T, c *etcd.Client) int {
	t.Helper()
	items, err := c.Range(context.Background(), etcd.Key("jobs")+"/")
	if err != nil {
		t.Fatalf("range jobs: %v", err)
	}
	return len(items)
}

// TestEnqueueTask_IdempotentReplay asserts a repeat enqueue with the same
// (user, key, hash) returns the original task id and creates no second task,
// even though the replay mints a fresh candidate id.
func TestEnqueueTask_IdempotentReplay(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	u := uuid.New()
	h := []byte{1, 2, 3}

	p := idemParams(u, "k1", h)
	id1, err := s.EnqueueTask(ctx, p, testJobArgs{})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	// A re-run mints a NEW candidate task id but the same descriptor.
	p.ID = uuid.New()
	id2, err := s.EnqueueTask(ctx, p, testJobArgs{})
	if err != nil {
		t.Fatalf("replay enqueue: %v", err)
	}
	if id2 != id1 {
		t.Errorf("replay returned %v, want the original %v", id2, id1)
	}
	if n := countTasks(t, s); n != 1 {
		t.Errorf("task count = %d, want 1 (replay must not create a second)", n)
	}
}

// TestEnqueueTask_HashMismatchFailsClosed asserts reusing a (user, key) with a
// different request hash fails closed with ErrIdempotencyKeyMismatch and does
// not create a second task.
func TestEnqueueTask_HashMismatchFailsClosed(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	u := uuid.New()

	if _, err := s.EnqueueTask(ctx, idemParams(u, "k1", []byte{1, 2, 3}), testJobArgs{}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	_, err := s.EnqueueTask(ctx, idemParams(u, "k1", []byte{9, 9, 9}), testJobArgs{})
	if !errors.Is(err, store.ErrIdempotencyKeyMismatch) {
		t.Errorf("mismatch enqueue = %v, want store.ErrIdempotencyKeyMismatch", err)
	}
	if n := countTasks(t, s); n != 1 {
		t.Errorf("task count = %d, want 1 (mismatch must not create a second)", n)
	}
}

// TestEnqueueTask_NoDescriptorUnchanged asserts the opt-out path (nil
// descriptor) is unchanged: two enqueues create two distinct tasks.
func TestEnqueueTask_NoDescriptorUnchanged(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	p1 := taskParams(store.TaskStatusPending, nil)
	p2 := taskParams(store.TaskStatusPending, nil)
	id1, err := s.EnqueueTask(ctx, p1, testJobArgs{})
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	id2, err := s.EnqueueTask(ctx, p2, testJobArgs{})
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if id1 == id2 {
		t.Errorf("two no-descriptor enqueues returned the same id %v", id1)
	}
	if n := countTasks(t, s); n != 2 {
		t.Errorf("task count = %d, want 2", n)
	}
}

// TestEnqueueTask_ConcurrentSameKey drives N real concurrent EnqueueTask calls
// against the embedded store, each with a fresh candidate id but the same
// (user, key, hash). Exactly one task and one job must survive and every caller
// must receive the same id: the winner's task.
func TestEnqueueTask_ConcurrentSameKey(t *testing.T) {
	s, c := startStore(t)
	ctx := context.Background()
	u := uuid.New()
	h := []byte{1, 2, 3}

	const n = 8
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			p := idemParams(u, "k1", h)
			p.ID = uuid.New()
			ids[i], errs[i] = s.EnqueueTask(ctx, p, testJobArgs{})
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Errorf("caller %d id = %v, want %v (all callers converge on the winner)", i, ids[i], ids[0])
		}
	}
	if got := countTasks(t, s); got != 1 {
		t.Errorf("task count = %d, want exactly 1", got)
	}
	if got := countJobs(t, c); got != 1 {
		t.Errorf("job count = %d, want exactly 1", got)
	}
}
