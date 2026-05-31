// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the idempotency middleware's storage contract.
var _ middleware.IdempotencyStore = (*etcdstore.Store)(nil)

func beginParams(key string, expires time.Time) store.BeginIdempotencyKeyParams {
	return store.BeginIdempotencyKeyParams{
		Key: key, RequestMethod: "POST", RequestPath: "/v1/vms", RequestHash: []byte("h"), ExpiresAt: expires,
	}
}

func TestIdempotencyBeginGetCompleteReplay(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)

	row, err := s.BeginIdempotencyKey(ctx, beginParams("k1", future))
	if err != nil || row.State != "in_flight" {
		t.Fatalf("Begin = (%+v, %v), want in_flight", row, err)
	}
	// Concurrent begin on the same key conflicts.
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("k1", future)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Begin conflict = %v, want store.ErrNotFound", err)
	}
	// Complete stores the response and flips to completed.
	status := int32(201)
	if err := s.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
		Key: "k1", ResponseStatus: &status, ResponseBody: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := s.GetIdempotencyKey(ctx, "k1")
	if err != nil || got.State != "completed" || got.ResponseStatus == nil || *got.ResponseStatus != 201 {
		t.Errorf("Get after complete = (%+v, %v), want completed/201", got, err)
	}
	// Delete on a completed row is a no-op (not ours to delete).
	if err := s.DeleteIdempotencyKey(ctx, "k1"); err != nil {
		t.Fatalf("Delete(completed): %v", err)
	}
	if _, err := s.GetIdempotencyKey(ctx, "k1"); err != nil {
		t.Errorf("completed row wrongly deleted: %v", err)
	}
}

func TestIdempotencyDeleteInFlight(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("k2", time.Now().UTC().Add(time.Hour))); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := s.DeleteIdempotencyKey(ctx, "k2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetIdempotencyKey(ctx, "k2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("in_flight row survived delete = %v, want ErrNotFound", err)
	}
}

func TestIdempotencyReclaimExpired(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("k3", past)); err != nil {
		t.Fatalf("Begin(expired): %v", err)
	}

	future := time.Now().UTC().Add(time.Hour)
	row, err := s.ReclaimIdempotencyKey(ctx, store.ReclaimIdempotencyKeyParams{
		Key: "k3", RequestMethod: "POST", RequestPath: "/v1/vms", RequestHash: []byte("h2"), ExpiresAt: future,
	})
	if err != nil || row.State != "in_flight" || !row.ExpiresAt.Equal(future) {
		t.Fatalf("Reclaim = (%+v, %v), want fresh in_flight", row, err)
	}
	// A second reclaim now fails - the row is no longer expired.
	if _, err := s.ReclaimIdempotencyKey(ctx, store.ReclaimIdempotencyKeyParams{
		Key: "k3", ExpiresAt: future,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Reclaim(not expired) = %v, want store.ErrNotFound", err)
	}
}

func TestIdempotencyDeleteExpiredSweep(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("old", time.Now().UTC().Add(-time.Hour))); err != nil {
		t.Fatalf("Begin(old): %v", err)
	}
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("new", time.Now().UTC().Add(time.Hour))); err != nil {
		t.Fatalf("Begin(new): %v", err)
	}
	deleted, err := s.DeleteExpiredIdempotencyKeys(ctx)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpired = (%d, %v), want 1", deleted, err)
	}
	if _, err := s.GetIdempotencyKey(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired row survived sweep")
	}
	if _, err := s.GetIdempotencyKey(ctx, "new"); err != nil {
		t.Errorf("fresh row wrongly swept: %v", err)
	}
}
