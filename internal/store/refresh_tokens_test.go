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

func createUserForTokens(t *testing.T, ctx context.Context, s *store.Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        uniqueEmail(),
		PasswordHash: "x",
		Role:         "viewer",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}

func TestRefreshTokensCreateAndGetByHash(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	tokenID := uuid.New()
	hash := []byte("hash-" + uuid.NewString())

	created, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID:        tokenID,
		UserID:    userID,
		TokenHash: hash,
		FamilyID:  tokenID, // first token in a fresh family
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	if created.ID != tokenID {
		t.Errorf("created.ID = %v, want %v", created.ID, tokenID)
	}
	if created.RevokedAt != nil {
		t.Errorf("fresh token RevokedAt = %v, want nil", created.RevokedAt)
	}

	got, err := s.Queries().GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash: %v", err)
	}
	if got.ID != tokenID {
		t.Errorf("got.ID = %v, want %v", got.ID, tokenID)
	}
}

func TestRefreshTokensRevoke(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	tokenID := uuid.New()
	hash := []byte("hash-" + uuid.NewString())

	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: tokenID, UserID: userID, TokenHash: hash, FamilyID: tokenID,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	if err := s.Queries().RevokeRefreshToken(ctx, tokenID); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}

	got, err := s.Queries().GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash after revoke: %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("RevokedAt = nil after revoke, want set")
	}

	// Idempotent: re-revoke is a no-op.
	if err := s.Queries().RevokeRefreshToken(ctx, tokenID); err != nil {
		t.Errorf("re-revoke = %v, want nil", err)
	}
}

func TestRefreshTokensRevokeFamily(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	familyID := uuid.New()

	// Three tokens in the same family.
	hashes := make([][]byte, 3)
	ids := make([]uuid.UUID, 3)
	for i := range hashes {
		ids[i] = uuid.New()
		hashes[i] = []byte("h-" + uuid.NewString())
		if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
			ID: ids[i], UserID: userID, TokenHash: hashes[i],
			FamilyID:  familyID,
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateRefreshToken[%d]: %v", i, err)
		}
	}
	// And one unrelated token (different family) that must NOT be revoked.
	otherID := uuid.New()
	otherHash := []byte("h-" + uuid.NewString())
	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: otherID, UserID: userID, TokenHash: otherHash,
		FamilyID:  otherID,
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken (other family): %v", err)
	}

	if err := s.Queries().RevokeRefreshTokenFamily(ctx, familyID); err != nil {
		t.Fatalf("RevokeRefreshTokenFamily: %v", err)
	}

	for i, h := range hashes {
		got, err := s.Queries().GetRefreshTokenByHash(ctx, h)
		if err != nil {
			t.Fatalf("GetRefreshTokenByHash[%d]: %v", i, err)
		}
		if got.RevokedAt == nil {
			t.Errorf("token %d in family RevokedAt = nil, want set", i)
		}
	}
	other, err := s.Queries().GetRefreshTokenByHash(ctx, otherHash)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash (other family): %v", err)
	}
	if other.RevokedAt != nil {
		t.Errorf("unrelated-family token RevokedAt = %v, want nil", other.RevokedAt)
	}
}

func TestRefreshTokensRevokeAllUser(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userA := createUserForTokens(t, ctx, s)
	userB := createUserForTokens(t, ctx, s)

	hashA := []byte("hA-" + uuid.NewString())
	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: userA, TokenHash: hashA, FamilyID: uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken userA: %v", err)
	}
	hashB := []byte("hB-" + uuid.NewString())
	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: userB, TokenHash: hashB, FamilyID: uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken userB: %v", err)
	}

	if err := s.Queries().RevokeAllUserRefreshTokens(ctx, userA); err != nil {
		t.Fatalf("RevokeAllUserRefreshTokens: %v", err)
	}

	gotA, err := s.Queries().GetRefreshTokenByHash(ctx, hashA)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash A: %v", err)
	}
	if gotA.RevokedAt == nil {
		t.Errorf("userA token RevokedAt = nil, want set")
	}
	gotB, err := s.Queries().GetRefreshTokenByHash(ctx, hashB)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash B: %v", err)
	}
	if gotB.RevokedAt != nil {
		t.Errorf("userB token RevokedAt = %v, want nil", gotB.RevokedAt)
	}
}

func TestRefreshTokensCascadeOnUserHardDelete(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	hash := []byte("h-" + uuid.NewString())
	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: userID, TokenHash: hash, FamilyID: uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	// Hard-delete the user via raw SQL — soft-delete (sets deleted_at) does
	// not trigger the FK ON DELETE CASCADE, which is the whole point of
	// the cascade: it only kicks in for genuine row removal.
	if _, err := s.Pool().Exec(ctx, `delete from users where id = $1`, userID); err != nil {
		t.Fatalf("hard-delete user: %v", err)
	}

	if _, err := s.Queries().GetRefreshTokenByHash(ctx, hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetRefreshTokenByHash after user hard-delete err = %v, want pgx.ErrNoRows", err)
	}
}

func TestRefreshTokensDeleteExpired(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	userID := createUserForTokens(t, ctx, s)
	freshHash := []byte("fresh-" + uuid.NewString())
	if _, err := s.Queries().CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: userID, TokenHash: freshHash, FamilyID: uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefreshToken fresh: %v", err)
	}

	// Insert a row whose expires_at is already in the past.
	if _, err := s.Pool().Exec(ctx,
		`insert into refresh_tokens (id, user_id, token_hash, family_id, expires_at)
         values (uuid_generate_v7(), $1, $2, uuid_generate_v7(), now() - interval '1 hour')`,
		userID, []byte("expired-"+uuid.NewString()),
	); err != nil {
		t.Fatalf("insert expired: %v", err)
	}

	deleted, err := s.Queries().DeleteExpiredRefreshTokens(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpiredRefreshTokens: %v", err)
	}
	if deleted < 1 {
		t.Errorf("DeleteExpiredRefreshTokens deleted %d, want >= 1", deleted)
	}

	// Fresh token survives.
	if _, err := s.Queries().GetRefreshTokenByHash(ctx, freshHash); err != nil {
		t.Errorf("fresh token vanished after cleanup: %v", err)
	}
}
