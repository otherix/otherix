// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestCasRefreshTokenUpdateRetriesOnStaleModRevision proves TouchRefreshToken's
// CAS has teeth against the theft cascade: a competing revoke (the family burn)
// that lands between casRefreshTokenUpdate's read and its commit must force the
// CAS to miss, re-read the now-revoked row, and preserve revoked_at while
// stamping last_used_at. A blind put would re-persist its stale revoked_at=nil
// snapshot, silently un-revoking a token the theft detection just burned.
func TestCasRefreshTokenUpdateRetriesOnStaleModRevision(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)

	tok, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TokenHash: []byte("cas-rt"),
		FamilyID:  uuid.New(),
		ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	now := time.Now().UTC()
	var attempts int
	out, err := s.casRefreshTokenUpdate(ctx, tok.ID, func(row *store.RefreshToken) {
		attempts++
		if attempts == 1 {
			// Competing theft-burn revoke between read and commit: it sets
			// revoked_at and bumps the row's ModRevision, so the pending CAS
			// misses and the mutate re-runs against the revoked row.
			if rerr := s.RevokeRefreshToken(ctx, tok.ID); rerr != nil {
				t.Fatalf("competing RevokeRefreshToken: %v", rerr)
			}
		}
		row.LastUsedAt = &now
	})
	if err != nil {
		t.Fatalf("casRefreshTokenUpdate: %v", err)
	}
	if attempts < 2 {
		t.Errorf("casRefreshTokenUpdate did not retry: attempts = %d, want >= 2", attempts)
	}
	if out.RevokedAt == nil {
		t.Errorf("returned RevokedAt = nil, want revoke preserved (token must stay burned)")
	}
	if out.LastUsedAt == nil {
		t.Errorf("returned LastUsedAt = nil, want stamped")
	}

	// Re-read: the revoke must survive the touch durably.
	got, err := s.RefreshTokenByHash(ctx, []byte("cas-rt"))
	if err != nil {
		t.Fatalf("RefreshTokenByHash: %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("persisted RevokedAt = nil, want revoke preserved")
	}
	if got.LastUsedAt == nil {
		t.Errorf("persisted LastUsedAt = nil, want stamped")
	}
}

// TestTouchRefreshTokenNoOpOnMissing pins the no-op contract the CAS conversion
// must preserve: a missing token returns nil without writing.
func TestTouchRefreshTokenNoOpOnMissing(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	if err := s.TouchRefreshToken(ctx, uuid.New()); err != nil {
		t.Errorf("TouchRefreshToken(missing) = %v, want nil", err)
	}
}
