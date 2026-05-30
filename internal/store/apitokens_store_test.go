// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// The tests in this file exercise the api-token domain methods on
// *Store backing the /v1/users/{id}/api-tokens surfaces: the
// APITokenByID not-found translation plus the create / list / revoke
// round-trips.

// seedAPIToken inserts an api token for the given user and returns the
// row. The token hash is derived from a fresh id so repeated seeds in
// the shared harness do not collide on the token-hash unique index.
func seedAPIToken(t *testing.T, ctx context.Context, s *store.Store, userID uuid.UUID, name string) store.ApiToken {
	t.Helper()
	id := uuid.New()
	row, err := s.CreateAPIToken(ctx, store.CreateApiTokenParams{
		ID:        id,
		UserID:    userID,
		Name:      name,
		TokenHash: append(id[:], id[:]...),
		Prefix:    id.String()[:8],
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return row
}

func TestAPITokenByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.APITokenByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("APITokenByID(absent) err = %v, want store.ErrNotFound", err)
	}
}

func TestCreateAndGetAPIToken(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	want := seedAPIToken(t, ctx, s, owner, "ci")

	got, err := s.APITokenByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("APITokenByID: %v", err)
	}
	if got.ID != want.ID || got.UserID != owner {
		t.Errorf("APITokenByID() = {ID:%v UserID:%v}, want {ID:%v UserID:%v}", got.ID, got.UserID, want.ID, owner)
	}
}

func TestListAPITokensByUser(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	want := seedAPIToken(t, ctx, s, owner, "ci")

	rows, err := s.ListAPITokensByUser(ctx, store.ListApiTokensByUserParams{
		UserID:     owner,
		LimitCount: 200,
	})
	if err != nil {
		t.Fatalf("ListAPITokensByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != want.ID {
		t.Errorf("ListAPITokensByUser = %d rows, want 1 with ID %v", len(rows), want.ID)
	}
}

func TestRevokeAPIToken(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	tok := seedAPIToken(t, ctx, s, owner, "ci")

	if err := s.RevokeAPIToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	got, err := s.APITokenByID(ctx, tok.ID)
	if err != nil {
		t.Fatalf("APITokenByID after revoke: %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("RevokedAt = nil after RevokeAPIToken, want set")
	}
}
