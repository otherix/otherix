// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the auth service contract + the cleanup seams.
var (
	_ auth.Store                    = (*etcdstore.Store)(nil)
	_ auth.RefreshTokenCleaner      = (*etcdstore.Store)(nil)
	_ middleware.IdempotencyCleaner = (*etcdstore.Store)(nil)
)

func rtParams(hash []byte, user, family uuid.UUID, expires time.Time) store.CreateRefreshTokenParams {
	return store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: hash, FamilyID: family, ExpiresAt: expires,
	}
}

func TestRefreshTokenCreateAndByHash(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	hash := []byte("hash-1")
	row, err := s.CreateRefreshToken(ctx, rtParams(hash, uuid.New(), uuid.New(), future))
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	got, err := s.RefreshTokenByHash(ctx, hash)
	if err != nil || got.ID != row.ID || got.RevokedAt != nil {
		t.Errorf("ByHash = (%+v, %v), want active row %v", got, err, row.ID)
	}
	if _, err := s.RefreshTokenByHash(ctx, []byte("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ByHash(missing) = %v, want ErrNotFound", err)
	}
}

func TestRevokeRefreshTokenIdempotent(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	hash := []byte("hash-revoke")
	row, _ := s.CreateRefreshToken(ctx, rtParams(hash, uuid.New(), uuid.New(), time.Now().UTC().Add(time.Hour)))

	if err := s.RevokeRefreshToken(ctx, row.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ := s.RefreshTokenByHash(ctx, hash)
	if got.RevokedAt == nil {
		t.Errorf("token not revoked")
	}
	// Idempotent re-revoke + missing id.
	if err := s.RevokeRefreshToken(ctx, row.ID); err != nil {
		t.Errorf("re-revoke = %v, want nil", err)
	}
	if err := s.RevokeRefreshToken(ctx, uuid.New()); err != nil {
		t.Errorf("revoke missing = %v, want nil", err)
	}
}

func TestRotateRefreshToken(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()
	family := uuid.New()
	parent, _ := s.CreateRefreshToken(ctx, rtParams([]byte("parent"), user, family, future))

	childHash := []byte("child")
	child, err := s.RotateRefreshToken(ctx, parent.ID, store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: childHash, FamilyID: family, ParentID: &parent.ID, ExpiresAt: future,
	})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// Parent revoked, child active + resolvable + chained.
	gotParent, _ := s.RefreshTokenByHash(ctx, []byte("parent"))
	if gotParent.RevokedAt == nil {
		t.Errorf("parent not revoked after rotation")
	}
	gotChild, err := s.RefreshTokenByHash(ctx, childHash)
	if err != nil || gotChild.ID != child.ID || gotChild.RevokedAt != nil || gotChild.FamilyID != family {
		t.Errorf("child = (%+v, %v), want active in family %v", gotChild, err, family)
	}
	if gotChild.ParentID == nil || *gotChild.ParentID != parent.ID {
		t.Errorf("child parent = %v, want %v", gotChild.ParentID, parent.ID)
	}
	// Rotating a missing parent fails.
	if _, err := s.RotateRefreshToken(ctx, uuid.New(), rtParams([]byte("x"), user, family, future)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("rotate missing parent = %v, want ErrNotFound", err)
	}
}

// TestRotateRefreshTokenRefusesRevokedParent guards the serialized half of the
// double-spend window (audit M8): once a parent has been rotated (revoked), a
// second rotation of the same parent must be refused with
// store.ErrRefreshTokenConflict and must not insert another live child, so the
// family ends with exactly one live token.
func TestRotateRefreshTokenRefusesRevokedParent(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()
	family := uuid.New()
	parent, err := s.CreateRefreshToken(ctx, rtParams([]byte("parent-cas"), user, family, future))
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	childA := store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("child-a"), FamilyID: family, ParentID: &parent.ID, ExpiresAt: future,
	}
	if _, err := s.RotateRefreshToken(ctx, parent.ID, childA); err != nil {
		t.Fatalf("first rotation: %v", err)
	}

	childB := store.CreateRefreshTokenParams{
		ID: uuid.New(), UserID: user, TokenHash: []byte("child-b"), FamilyID: family, ParentID: &parent.ID, ExpiresAt: future,
	}
	if _, err := s.RotateRefreshToken(ctx, parent.ID, childB); !errors.Is(err, store.ErrRefreshTokenConflict) {
		t.Errorf("second rotation = %v, want ErrRefreshTokenConflict", err)
	}
	if _, err := s.RefreshTokenByHash(ctx, []byte("child-b")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("childB lookup = %v, want ErrNotFound (childB must not be inserted)", err)
	}
	// The family holds exactly one live token: childA.
	gotParent, _ := s.RefreshTokenByHash(ctx, []byte("parent-cas"))
	if gotParent.RevokedAt == nil {
		t.Errorf("parent not revoked after rotation")
	}
	gotA, err := s.RefreshTokenByHash(ctx, []byte("child-a"))
	if err != nil || gotA.RevokedAt != nil {
		t.Errorf("childA = (%+v, %v), want live", gotA, err)
	}
}

// TestRotateRefreshTokenConcurrentSingleWinner guards the simultaneous half of
// the double-spend window (audit M8): N concurrent rotations of the same live
// parent must produce exactly one live child; every loser observes
// store.ErrRefreshTokenConflict. The CAS on the parent's ModRevision makes the
// invariant hold regardless of scheduling.
func TestRotateRefreshTokenConcurrentSingleWinner(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()
	family := uuid.New()
	parent, err := s.CreateRefreshToken(ctx, rtParams([]byte("parent-race"), user, family, future))
	if err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	const n = 8
	errs := make([]error, n)
	hashes := make([][]byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		hashes[i] = []byte(fmt.Sprintf("race-child-%d", i))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.RotateRefreshToken(ctx, parent.ID, store.CreateRefreshTokenParams{
				ID: uuid.New(), UserID: user, TokenHash: hashes[i], FamilyID: family, ParentID: &parent.ID, ExpiresAt: future,
			})
		}(i)
	}
	wg.Wait()

	var winners, conflicts int
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrRefreshTokenConflict):
			conflicts++
		default:
			t.Errorf("rotation %d = %v, want nil or ErrRefreshTokenConflict", i, err)
		}
	}
	if winners != 1 || conflicts != n-1 {
		t.Errorf("winners = %d, conflicts = %d, want 1 and %d", winners, conflicts, n-1)
	}

	// Exactly one live child exists; every loser's child is absent.
	var live int
	for i := range hashes {
		got, err := s.RefreshTokenByHash(ctx, hashes[i])
		switch {
		case err == nil && got.RevokedAt == nil:
			live++
		case errors.Is(err, store.ErrNotFound):
		default:
			t.Errorf("child %d lookup = (%+v, %v)", i, got, err)
		}
	}
	if live != 1 {
		t.Errorf("live children = %d, want exactly 1", live)
	}
	gotParent, _ := s.RefreshTokenByHash(ctx, []byte("parent-race"))
	if gotParent.RevokedAt == nil {
		t.Errorf("parent not revoked after concurrent rotations")
	}
}

func TestRevokeRefreshTokenFamilyTheftDetection(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	user := uuid.New()
	stolen := uuid.New() // the compromised family
	other := uuid.New()  // an unrelated family that must survive

	hashes := [][]byte{[]byte("f1"), []byte("f2"), []byte("f3")}
	for _, h := range hashes {
		if _, err := s.CreateRefreshToken(ctx, rtParams(h, user, stolen, future)); err != nil {
			t.Fatalf("create family token: %v", err)
		}
	}
	if _, err := s.CreateRefreshToken(ctx, rtParams([]byte("safe"), user, other, future)); err != nil {
		t.Fatalf("create other token: %v", err)
	}

	// Replay detected -> burn the whole family.
	if err := s.RevokeRefreshTokenFamily(ctx, stolen); err != nil {
		t.Fatalf("RevokeRefreshTokenFamily: %v", err)
	}
	for _, h := range hashes {
		got, _ := s.RefreshTokenByHash(ctx, h)
		if got.RevokedAt == nil {
			t.Errorf("family token %s not revoked", h)
		}
	}
	safe, _ := s.RefreshTokenByHash(ctx, []byte("safe"))
	if safe.RevokedAt != nil {
		t.Errorf("unrelated family token wrongly revoked")
	}
}

func TestRevokeAllUserRefreshTokens(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	alice := uuid.New()
	bob := uuid.New()
	s.CreateRefreshToken(ctx, rtParams([]byte("a1"), alice, uuid.New(), future))
	s.CreateRefreshToken(ctx, rtParams([]byte("a2"), alice, uuid.New(), future))
	s.CreateRefreshToken(ctx, rtParams([]byte("b1"), bob, uuid.New(), future))

	if err := s.RevokeAllUserRefreshTokens(ctx, alice); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	for _, h := range [][]byte{[]byte("a1"), []byte("a2")} {
		got, _ := s.RefreshTokenByHash(ctx, h)
		if got.RevokedAt == nil {
			t.Errorf("alice token %s not revoked", h)
		}
	}
	b, _ := s.RefreshTokenByHash(ctx, []byte("b1"))
	if b.RevokedAt != nil {
		t.Errorf("bob's token wrongly revoked")
	}
}

func TestDeleteExpiredRefreshTokens(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	user := uuid.New()
	s.CreateRefreshToken(ctx, rtParams([]byte("expired"), user, uuid.New(), time.Now().UTC().Add(-time.Hour)))
	s.CreateRefreshToken(ctx, rtParams([]byte("fresh"), user, uuid.New(), time.Now().UTC().Add(time.Hour)))

	deleted, err := s.DeleteExpiredRefreshTokens(ctx, time.Now().UTC())
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpired = (%d, %v), want 1", deleted, err)
	}
	if _, err := s.RefreshTokenByHash(ctx, []byte("expired")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired token (or its hash index) survived")
	}
	if _, err := s.RefreshTokenByHash(ctx, []byte("fresh")); err != nil {
		t.Errorf("fresh token wrongly swept: %v", err)
	}
}
