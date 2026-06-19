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

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the idempotency middleware's storage contract.
var _ middleware.IdempotencyStore = (*etcdstore.Store)(nil)

// idempUser is a fixed user id used to scope the per-user idempotency rows in
// these store tests.
var idempUser = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func beginParams(key string, expires time.Time) store.BeginIdempotencyKeyParams {
	uid := idempUser
	return store.BeginIdempotencyKeyParams{
		Key: key, UserID: &uid, RequestMethod: "POST", RequestPath: "/v1/vms", RequestHash: []byte("h"), ExpiresAt: expires,
	}
}

func TestIdempotencyBeginGetCompleteReplay(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	uid := idempUser

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
		Key: "k1", UserID: &uid, ResponseStatus: &status, ResponseBody: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := s.GetIdempotencyKey(ctx, uid, "k1")
	if err != nil || got.State != "completed" || got.ResponseStatus == nil || *got.ResponseStatus != 201 {
		t.Errorf("Get after complete = (%+v, %v), want completed/201", got, err)
	}
	// Delete on a completed row is a no-op (not ours to delete).
	if err := s.DeleteIdempotencyKey(ctx, uid, "k1"); err != nil {
		t.Fatalf("Delete(completed): %v", err)
	}
	if _, err := s.GetIdempotencyKey(ctx, uid, "k1"); err != nil {
		t.Errorf("completed row wrongly deleted: %v", err)
	}
}

func TestIdempotencyDeleteInFlight(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	uid := idempUser
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("k2", time.Now().UTC().Add(time.Hour))); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := s.DeleteIdempotencyKey(ctx, uid, "k2"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetIdempotencyKey(ctx, uid, "k2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("in_flight row survived delete = %v, want ErrNotFound", err)
	}
}

func TestIdempotencyReclaimExpired(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	uid := idempUser
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := s.BeginIdempotencyKey(ctx, beginParams("k3", past)); err != nil {
		t.Fatalf("Begin(expired): %v", err)
	}

	future := time.Now().UTC().Add(time.Hour)
	row, err := s.ReclaimIdempotencyKey(ctx, store.ReclaimIdempotencyKeyParams{
		Key: "k3", UserID: &uid, RequestMethod: "POST", RequestPath: "/v1/vms", RequestHash: []byte("h2"), ExpiresAt: future,
	})
	if err != nil || row.State != "in_flight" || !row.ExpiresAt.Equal(future) {
		t.Fatalf("Reclaim = (%+v, %v), want fresh in_flight", row, err)
	}
	// A second reclaim now fails - the row is no longer expired.
	if _, err := s.ReclaimIdempotencyKey(ctx, store.ReclaimIdempotencyKeyParams{
		Key: "k3", UserID: &uid, ExpiresAt: future,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Reclaim(not expired) = %v, want store.ErrNotFound", err)
	}
}

// TestIdempotencyPerUserKeyNamespace asserts the per-user scope at the store
// layer: two users using the same key string each get an independent row.
func TestIdempotencyPerUserKeyNamespace(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	userA := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	userB := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	beginFor := func(uid uuid.UUID) store.BeginIdempotencyKeyParams {
		u := uid
		return store.BeginIdempotencyKeyParams{
			Key: "shared", UserID: &u, RequestMethod: "POST", RequestPath: "/v1/vms", RequestHash: []byte("h"), ExpiresAt: future,
		}
	}

	if _, err := s.BeginIdempotencyKey(ctx, beginFor(userA)); err != nil {
		t.Fatalf("Begin(userA): %v", err)
	}
	// User B begins the same key string: it must NOT conflict with user A.
	if _, err := s.BeginIdempotencyKey(ctx, beginFor(userB)); err != nil {
		t.Fatalf("Begin(userB) = %v, want success (independent namespace)", err)
	}
	// Each sees only their own row.
	if _, err := s.GetIdempotencyKey(ctx, userA, "shared"); err != nil {
		t.Errorf("Get(userA, shared) = %v, want found", err)
	}
	if _, err := s.GetIdempotencyKey(ctx, userB, "shared"); err != nil {
		t.Errorf("Get(userB, shared) = %v, want found", err)
	}
}

func TestIdempotencyDeleteExpiredSweep(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	uid := idempUser
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
	if _, err := s.GetIdempotencyKey(ctx, uid, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired row survived sweep")
	}
	if _, err := s.GetIdempotencyKey(ctx, uid, "new"); err != nil {
		t.Errorf("fresh row wrongly swept: %v", err)
	}
}
