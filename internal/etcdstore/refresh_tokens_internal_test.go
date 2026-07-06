// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// countActiveFamily returns how many tokens in the family are active (not
// revoked), reading the family index directly.
func countActiveFamily(t *testing.T, s *Store, family uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	items, err := s.c.Range(ctx, refreshTokenFamilyIndexPrefix(family))
	if err != nil {
		t.Fatalf("range family index: %v", err)
	}
	active := 0
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			t.Fatalf("bad family index value %q: %v", kv.Value, perr)
		}
		var row store.RefreshToken
		found, gerr := s.c.GetJSON(ctx, refreshTokenKey(id), &row)
		if gerr != nil {
			t.Fatalf("get token %s: %v", id, gerr)
		}
		if found && row.RevokedAt == nil {
			active++
		}
	}
	return active
}

// TestRevokeFamilyBarrierBlocksConcurrentRotation is the seam test: a rotation
// chain racing the family burn must never leave an active token behind. The
// pre-barrier single-snapshot revokeIndexed lets a child inserted after its
// range survive; the family-burn barrier bars the family before sweeping, so
// every racing rotation is CAS-refused and no active token can remain. Repeated
// runs make the interleaving reliable.
func TestRevokeFamilyBarrierBlocksConcurrentRotation(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()

	for run := 0; run < 20; run++ {
		family := uuid.New()
		parent, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
			ID: uuid.New(), UserID: user, TokenHash: []byte(fmt.Sprintf("bp-%d", run)),
			FamilyID: family, ExpiresAt: future,
		})
		if err != nil {
			t.Fatalf("run %d CreateRefreshToken: %v", run, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		// Rotator: chain-rotate the family as fast as it can, racing the burn.
		go func() {
			defer wg.Done()
			cur := parent.ID
			for i := 0; i < 64; i++ {
				next, rerr := s.RotateRefreshToken(ctx, cur, store.CreateRefreshTokenParams{
					ID: uuid.New(), UserID: user, TokenHash: []byte(fmt.Sprintf("bc-%d-%d", run, i)),
					FamilyID: family, ParentID: &cur, ExpiresAt: future,
				})
				if rerr != nil {
					return // parent revoked or family barred: chain ends
				}
				cur = next.ID
			}
		}()
		// Burn: fire the theft cascade concurrently.
		go func() {
			defer wg.Done()
			if berr := s.RevokeRefreshTokenFamily(ctx, family); berr != nil {
				t.Errorf("run %d RevokeRefreshTokenFamily: %v", run, berr)
			}
		}()
		wg.Wait()

		if active := countActiveFamily(t, s, family); active != 0 {
			t.Fatalf("run %d: %d active tokens survived the family burn, want 0", run, active)
		}
	}
}

// TestRotateRefreshTokenRefusedByFamilyBarrier proves a barred family cannot be
// rotated: the barrier compare in the CAS fails, no child is written.
func TestRotateRefreshTokenRefusedByFamilyBarrier(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user, family := uuid.New(), uuid.New()

	parent, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("barred-parent"), FamilyID: family, ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	if err := s.putFamilyBarrier(ctx, family); err != nil {
		t.Fatalf("putFamilyBarrier: %v", err)
	}

	_, err = s.RotateRefreshToken(ctx, parent.ID, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("barred-child"), FamilyID: family, ParentID: &parent.ID, ExpiresAt: future,
	})
	if !errors.Is(err, store.ErrRefreshTokenConflict) {
		t.Errorf("RotateRefreshToken into barred family = %v, want ErrRefreshTokenConflict", err)
	}
	if _, err := s.RefreshTokenByHash(ctx, []byte("barred-child")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("barred rotation still inserted a child: RefreshTokenByHash = %v, want ErrNotFound", err)
	}
}

// TestBurnFamiliesAndSweepScopedToListedFamilies proves the sweep is scoped to
// the barred families, not the user: a family NOT in the list keeps its active
// token and stays rotatable. This is the logout-all "a fresh login survives"
// guarantee - a user-scoped sweep would wrongly revoke the other family's token.
func TestBurnFamiliesAndSweepScopedToListedFamilies(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()
	f1, f2 := uuid.New(), uuid.New()

	t1, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("s-t1"), FamilyID: f1, ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("create t1: %v", err)
	}
	t2, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("s-t2"), FamilyID: f2, ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("create t2: %v", err)
	}

	// Burn only f1.
	if err := s.burnFamiliesAndSweep(ctx, []uuid.UUID{f1}); err != nil {
		t.Fatalf("burnFamiliesAndSweep: %v", err)
	}

	// f1's token is revoked and f1 is barred.
	if got, _ := s.RefreshTokenByHash(ctx, []byte("s-t1")); got.RevokedAt == nil {
		t.Errorf("t1 not revoked after burning f1")
	}
	if _, err := s.RotateRefreshToken(ctx, t1.ID, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("s-t1c"), FamilyID: f1, ParentID: &t1.ID, ExpiresAt: future,
	}); !errors.Is(err, store.ErrRefreshTokenConflict) {
		t.Errorf("rotate barred f1 = %v, want ErrRefreshTokenConflict", err)
	}

	// f2 is untouched: token active, family rotatable.
	if got, _ := s.RefreshTokenByHash(ctx, []byte("s-t2")); got.RevokedAt != nil {
		t.Errorf("t2 wrongly revoked - sweep leaked into f2")
	}
	if _, err := s.RotateRefreshToken(ctx, t2.ID, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("s-t2c"), FamilyID: f2, ParentID: &t2.ID, ExpiresAt: future,
	}); err != nil {
		t.Errorf("rotate unbarred f2 = %v, want success", err)
	}
}

// TestRevokeAllUserBarsAllFamiliesButNotFreshLogin drives the full logout-all
// path: it bars and revokes every family the user currently holds, but a family
// minted by a fresh login afterwards is neither barred nor revoked and rotates
// normally.
func TestRevokeAllUserBarsAllFamiliesButNotFreshLogin(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()
	f1, f2 := uuid.New(), uuid.New()

	t1, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("la-t1"), FamilyID: f1, ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("create t1: %v", err)
	}
	if _, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("la-t2"), FamilyID: f2, ExpiresAt: future,
	}); err != nil {
		t.Fatalf("create t2: %v", err)
	}

	if err := s.RevokeAllUserRefreshTokens(ctx, user); err != nil {
		t.Fatalf("RevokeAllUserRefreshTokens: %v", err)
	}

	// Both current families are revoked and barred.
	for _, h := range [][]byte{[]byte("la-t1"), []byte("la-t2")} {
		if got, _ := s.RefreshTokenByHash(ctx, h); got.RevokedAt == nil {
			t.Errorf("token %s not revoked by logout-all", h)
		}
	}
	if _, err := s.RotateRefreshToken(ctx, t1.ID, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("la-t1c"), FamilyID: f1, ParentID: &t1.ID, ExpiresAt: future,
	}); !errors.Is(err, store.ErrRefreshTokenConflict) {
		t.Errorf("rotate barred family after logout-all = %v, want ErrRefreshTokenConflict", err)
	}

	// A fresh login (new family) is untouched and rotates normally.
	f3 := uuid.New()
	t3, err := s.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("la-t3"), FamilyID: f3, ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("fresh login create: %v", err)
	}
	if _, err := s.RotateRefreshToken(ctx, t3.ID, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("la-t3c"), FamilyID: f3, ParentID: &t3.ID, ExpiresAt: future,
	}); err != nil {
		t.Errorf("rotate fresh-login family after logout-all = %v, want success", err)
	}
}

// TestFamilyBarrierExpiryCleanup proves DeleteExpiredRefreshTokens reaps a
// barrier past its ExpiresAt and leaves a still-valid one.
func TestFamilyBarrierExpiryCleanup(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	stale, live := uuid.New(), uuid.New()

	writeBarrier := func(family uuid.UUID, exp time.Time) {
		val, err := etcd.Marshal(familyBarrier{ExpiresAt: exp})
		if err != nil {
			t.Fatalf("marshal barrier: %v", err)
		}
		if err := s.c.Put(ctx, refreshFamilyBarrierKey(family), val); err != nil {
			t.Fatalf("put barrier: %v", err)
		}
	}
	writeBarrier(stale, now.Add(-time.Hour))
	writeBarrier(live, now.Add(time.Hour))

	if _, err := s.DeleteExpiredRefreshTokens(ctx, now); err != nil {
		t.Fatalf("DeleteExpiredRefreshTokens: %v", err)
	}

	if _, found, _ := s.c.Get(ctx, refreshFamilyBarrierKey(stale)); found {
		t.Errorf("stale barrier not reaped")
	}
	if _, found, _ := s.c.Get(ctx, refreshFamilyBarrierKey(live)); !found {
		t.Errorf("live barrier wrongly reaped")
	}
}

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
