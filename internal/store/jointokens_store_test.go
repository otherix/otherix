// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// The tests in this file exercise the cluster-CA and join-token domain
// methods on *Store: the ActiveCACert / JoinTokenByID not-found
// translation and the join-token create / list / revoke round-trips
// that back the /v1/ca, /v1/nodes/join-tokens and /v1/nodes/join
// handlers.

// seedActiveCA inserts a fresh active ca_certs row, clearing any
// existing active row first to satisfy the at-most-one-active partial
// unique index. The row is deactivated again on cleanup so the shared
// harness does not leak an active CA into later tests.
func seedActiveCA(t *testing.T, ctx context.Context, s *store.Store, fingerprint []byte) store.CaCert {
	t.Helper()
	if err := s.Queries().DeactivateCACerts(ctx); err != nil {
		t.Fatalf("deactivate existing CA: %v", err)
	}
	now := time.Now().UTC()
	row, err := s.Queries().CreateCACert(ctx, store.CreateCACertParams{
		ID:                uuid.New(),
		CertPem:           []byte("-----BEGIN CERTIFICATE-----\nstub\n-----END CERTIFICATE-----\n"),
		KeyPem:            []byte("-----BEGIN PRIVATE KEY-----\nstub\n-----END PRIVATE KEY-----\n"),
		FingerprintSha256: fingerprint,
		NotBefore:         now,
		NotAfter:          now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("seed CA cert: %v", err)
	}
	t.Cleanup(func() { _ = s.Queries().DeactivateCACerts(context.Background()) })
	return row
}

func TestActiveCACertNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if err := s.Queries().DeactivateCACerts(ctx); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := s.ActiveCACert(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ActiveCACert(none active) err = %v, want store.ErrNotFound", err)
	}
}

func TestActiveCACertFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	fp := bytes.Repeat([]byte{0xAB}, 32)
	want := seedActiveCA(t, ctx, s, fp)

	got, err := s.ActiveCACert(ctx)
	if err != nil {
		t.Fatalf("ActiveCACert: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ActiveCACert().ID = %v, want %v", got.ID, want.ID)
	}
	if !bytes.Equal(got.FingerprintSha256, fp) {
		t.Errorf("ActiveCACert().FingerprintSha256 = %x, want %x", got.FingerprintSha256, fp)
	}
}

func TestJoinTokenByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.JoinTokenByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("JoinTokenByID(absent) err = %v, want store.ErrNotFound", err)
	}
}

// seedJoinToken inserts a join token expiring in one hour and returns
// the row.
func seedJoinToken(t *testing.T, ctx context.Context, s *store.Store, creator uuid.UUID) store.JoinToken {
	t.Helper()
	id := uuid.New()
	// Derive a per-token-unique 32-byte hash from the id so repeated
	// seeds in the shared harness do not collide on uq_join_tokens_hash.
	hash := append(id[:], id[:]...)
	row, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:              id,
		TokenHash:       hash,
		CreatedByUserID: &creator,
		ExpiresAt:       time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	return row
}

func TestCreateAndGetJoinToken(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	want := seedJoinToken(t, ctx, s, creator)

	got, err := s.JoinTokenByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("JoinTokenByID: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("JoinTokenByID().ID = %v, want %v", got.ID, want.ID)
	}
}

func TestListJoinTokensIncludesCreated(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	want := seedJoinToken(t, ctx, s, creator)

	rows, err := s.ListJoinTokens(ctx, store.ListJoinTokensParams{
		IncludeExpired: true,
		LimitCount:     200,
	})
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == want.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListJoinTokens did not include created token %v", want.ID)
	}
}

func TestRevokeJoinTokenExpiresIt(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	tok := seedJoinToken(t, ctx, s, creator)

	if err := s.RevokeJoinToken(ctx, tok.ID); err != nil {
		t.Fatalf("RevokeJoinToken: %v", err)
	}
	got, err := s.JoinTokenByID(ctx, tok.ID)
	if err != nil {
		t.Fatalf("JoinTokenByID after revoke: %v", err)
	}
	if got.ExpiresAt.After(time.Now()) {
		t.Errorf("revoked token ExpiresAt = %v, want in the past", got.ExpiresAt)
	}
}

func TestListJoinTokenConsumptionsEmpty(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	tok := seedJoinToken(t, ctx, s, creator)

	rows, err := s.ListJoinTokenConsumptions(ctx, store.ListJoinTokenConsumptionsParams{
		JoinTokenID: tok.ID,
		LimitCount:  200,
	})
	if err != nil {
		t.Fatalf("ListJoinTokenConsumptions: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListJoinTokenConsumptions on fresh token = %d rows, want 0", len(rows))
	}
}
