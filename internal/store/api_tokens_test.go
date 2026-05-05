// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

func TestApiTokensCreateAndGetByHash(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	hash := []byte("hash-" + uuid.NewString())

	created, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID:        uuid.New(),
		UserID:    userID,
		Name:      "ci-token",
		TokenHash: hash,
		Prefix:    "otx_abcd",
	})
	if err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}
	if created.Prefix != "otx_abcd" {
		t.Errorf("created.Prefix = %q, want %q", created.Prefix, "otx_abcd")
	}
	if created.ExpiresAt != nil {
		t.Errorf("created.ExpiresAt = %v, want nil (no expiry passed)", created.ExpiresAt)
	}
	if len(created.Scopes) != 0 {
		t.Errorf("created.Scopes = %v, want empty slice", created.Scopes)
	}

	got, err := s.Queries().GetApiTokenByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetApiTokenByHash: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("got.ID = %v, want %v", got.ID, created.ID)
	}
}

func TestApiTokensGetByHashHidesRevokedAndExpired(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)

	// Revoked token: hash lookup must return ErrNoRows.
	revokedID := uuid.New()
	revokedHash := []byte("rev-" + uuid.NewString())
	if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID: revokedID, UserID: userID, Name: "r", TokenHash: revokedHash, Prefix: "otx_rev0",
	}); err != nil {
		t.Fatalf("CreateApiToken revoked: %v", err)
	}
	if err := s.Queries().RevokeApiToken(ctx, revokedID); err != nil {
		t.Fatalf("RevokeApiToken: %v", err)
	}
	if _, err := s.Queries().GetApiTokenByHash(ctx, revokedHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetApiTokenByHash(revoked) err = %v, want pgx.ErrNoRows", err)
	}

	// Expired token: same — already-past expires_at is excluded.
	pastExpiry := time.Now().Add(-time.Hour)
	expiredHash := []byte("exp-" + uuid.NewString())
	if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID: uuid.New(), UserID: userID, Name: "e", TokenHash: expiredHash, Prefix: "otx_exp0",
		ExpiresAt: &pastExpiry,
	}); err != nil {
		t.Fatalf("CreateApiToken expired: %v", err)
	}
	if _, err := s.Queries().GetApiTokenByHash(ctx, expiredHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetApiTokenByHash(expired) err = %v, want pgx.ErrNoRows", err)
	}

	// GetApiTokenByID still finds them — UI/admin needs to display revoked rows.
	if _, err := s.Queries().GetApiTokenByID(ctx, revokedID); err != nil {
		t.Errorf("GetApiTokenByID(revoked): %v", err)
	}
}

func TestApiTokensListByUser(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userA := createUserForTokens(t, ctx, s)
	userB := createUserForTokens(t, ctx, s)

	for i := 0; i < 3; i++ {
		if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
			ID: uuid.New(), UserID: userA, Name: "tA",
			TokenHash: []byte("hA-" + uuid.NewString()), Prefix: "otx_a000",
		}); err != nil {
			t.Fatalf("CreateApiToken A%d: %v", i, err)
		}
	}
	if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID: uuid.New(), UserID: userB, Name: "tB",
		TokenHash: []byte("hB-" + uuid.NewString()), Prefix: "otx_b000",
	}); err != nil {
		t.Fatalf("CreateApiToken B: %v", err)
	}

	got, err := s.Queries().ListApiTokensByUser(ctx, store.ListApiTokensByUserParams{
		UserID:     userA,
		LimitCount: 10,
	})
	if err != nil {
		t.Fatalf("ListApiTokensByUser: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("ListApiTokensByUser len = %d, want 3", len(got))
	}
	for _, row := range got {
		if row.UserID != userA {
			t.Errorf("row.UserID = %v, want %v (cross-user leakage)", row.UserID, userA)
		}
	}
}

func TestApiTokensTouch(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	id := uuid.New()
	if _, err := s.Queries().CreateApiToken(ctx, store.CreateApiTokenParams{
		ID: id, UserID: userID, Name: "t",
		TokenHash: []byte("h-" + uuid.NewString()), Prefix: "otx_xxxx",
	}); err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}

	if err := s.Queries().TouchApiToken(ctx, id); err != nil {
		t.Fatalf("TouchApiToken: %v", err)
	}

	got, err := s.Queries().GetApiTokenByID(ctx, id)
	if err != nil {
		t.Fatalf("GetApiTokenByID: %v", err)
	}
	if got.LastUsedAt == nil {
		t.Errorf("LastUsedAt = nil after Touch, want set")
	}
}
