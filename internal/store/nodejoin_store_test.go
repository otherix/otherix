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

	"github.com/otherix/otherix/internal/store"
)

// The tests in this file exercise the atomic Step 2 join-token
// redemption seam (RedeemJoinToken) backing POST /v1/nodes/join: the
// happy-path node + agent-cert + consumption writes, the four domain
// rejection sentinels, and the sign-callback rollback contract.

// fakeIssuedCert returns a store.IssuedCert with per-call-unique serial
// and fingerprint so repeated redemptions in the shared harness do not
// collide on agent_certs uniqueness.
func fakeIssuedCert() store.IssuedCert {
	id := uuid.New()
	now := time.Now().UTC()
	return store.IssuedCert{
		Serial:            id[:],
		FingerprintSha256: append(id[:], id[:]...),
		SubjectDN:         "CN=node-test",
		NotBefore:         now.Add(-time.Hour),
		NotAfter:          now.Add(24 * time.Hour),
	}
}

// redeemParams builds a RedeemJoinTokenParams for a fresh node name with
// the given token hash.
func redeemParams(tokenHash []byte, nodeName string) store.RedeemJoinTokenParams {
	return store.RedeemJoinTokenParams{
		TokenHash:               tokenHash,
		NodeName:                nodeName,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + nodeName + ":9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
	}
}

// seedJoinTokenWith inserts a join token with explicit max_uses /
// intended_node_name and returns the row plus its hash.
func seedJoinTokenWith(t *testing.T, ctx context.Context, s *store.Store, creator uuid.UUID, maxUses *int32, intended *string) ([]byte, store.JoinToken) {
	t.Helper()
	id := uuid.New()
	hash := append(id[:], id[:]...)
	row, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:               id,
		TokenHash:        hash,
		CreatedByUserID:  &creator,
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
		MaxUses:          maxUses,
		IntendedNodeName: intended,
	})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	return hash, row
}

func TestRedeemJoinTokenHappyPath(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	hash, tok := seedJoinTokenWith(t, ctx, s, creator, nil, nil)
	name := "node-" + uuid.NewString()[:8]

	var signedNode store.Node
	res, err := s.RedeemJoinToken(ctx, redeemParams(hash, name), func(n store.Node) (store.IssuedCert, error) {
		signedNode = n
		return fakeIssuedCert(), nil
	})
	if err != nil {
		t.Fatalf("RedeemJoinToken: %v", err)
	}
	if signedNode.Name != name {
		t.Errorf("sign callback node.Name = %q, want %q", signedNode.Name, name)
	}
	if res.TokenID != tok.ID {
		t.Errorf("result.TokenID = %v, want %v", res.TokenID, tok.ID)
	}
	// Node row exists.
	if _, err := s.NodeByName(ctx, name); err != nil {
		t.Errorf("NodeByName after redeem: %v", err)
	}
	// Consumption recorded.
	cons, err := s.ListJoinTokenConsumptions(ctx, store.ListJoinTokenConsumptionsParams{JoinTokenID: tok.ID, LimitCount: 10})
	if err != nil {
		t.Fatalf("ListJoinTokenConsumptions: %v", err)
	}
	if len(cons) != 1 {
		t.Errorf("consumptions = %d, want 1", len(cons))
	}
}

func TestRedeemJoinTokenUnknownToken(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	bogus := uuid.New()
	_, err := s.RedeemJoinToken(ctx, redeemParams(bogus[:], "node-"+uuid.NewString()[:8]),
		func(store.Node) (store.IssuedCert, error) { return fakeIssuedCert(), nil })
	if !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("RedeemJoinToken(unknown) err = %v, want store.ErrJoinTokenInvalid", err)
	}
}

func TestRedeemJoinTokenExhausted(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	one := int32(1)
	hash, _ := seedJoinTokenWith(t, ctx, s, creator, &one, nil)

	// First redemption consumes the single use.
	if _, err := s.RedeemJoinToken(ctx, redeemParams(hash, "node-"+uuid.NewString()[:8]),
		func(store.Node) (store.IssuedCert, error) { return fakeIssuedCert(), nil }); err != nil {
		t.Fatalf("first RedeemJoinToken: %v", err)
	}
	// Second redemption exceeds max_uses.
	_, err := s.RedeemJoinToken(ctx, redeemParams(hash, "node-"+uuid.NewString()[:8]),
		func(store.Node) (store.IssuedCert, error) { return fakeIssuedCert(), nil })
	if !errors.Is(err, store.ErrJoinTokenExhausted) {
		t.Errorf("RedeemJoinToken(exhausted) err = %v, want store.ErrJoinTokenExhausted", err)
	}
}

func TestRedeemJoinTokenNodeNameMismatch(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	one := int32(1)
	intended := "node-bound-" + uuid.NewString()[:8]
	hash, _ := seedJoinTokenWith(t, ctx, s, creator, &one, &intended)

	_, err := s.RedeemJoinToken(ctx, redeemParams(hash, "node-other-"+uuid.NewString()[:8]),
		func(store.Node) (store.IssuedCert, error) { return fakeIssuedCert(), nil })
	if !errors.Is(err, store.ErrJoinNodeNameMismatch) {
		t.Errorf("RedeemJoinToken(mismatch) err = %v, want store.ErrJoinNodeNameMismatch", err)
	}
}

func TestRedeemJoinTokenNodeNameTaken(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	name := "node-" + uuid.NewString()[:8]

	// First redemption creates the node + active cert.
	hash1, _ := seedJoinTokenWith(t, ctx, s, creator, nil, nil)
	if _, err := s.RedeemJoinToken(ctx, redeemParams(hash1, name),
		func(store.Node) (store.IssuedCert, error) { return fakeIssuedCert(), nil }); err != nil {
		t.Fatalf("first RedeemJoinToken: %v", err)
	}
	// Second redemption for the same name, now holding an active cert.
	hash2, _ := seedJoinTokenWith(t, ctx, s, creator, nil, nil)
	_, err := s.RedeemJoinToken(ctx, redeemParams(hash2, name),
		func(store.Node) (store.IssuedCert, error) { return fakeIssuedCert(), nil })
	if !errors.Is(err, store.ErrJoinNodeNameTaken) {
		t.Errorf("RedeemJoinToken(taken) err = %v, want store.ErrJoinNodeNameTaken", err)
	}
}

func TestRedeemJoinTokenSignErrorRollsBack(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	creator := seedUser(t, ctx, s, "admin")
	hash, tok := seedJoinTokenWith(t, ctx, s, creator, nil, nil)
	name := "node-" + uuid.NewString()[:8]

	signErr := errors.New("sign failed")
	_, err := s.RedeemJoinToken(ctx, redeemParams(hash, name),
		func(store.Node) (store.IssuedCert, error) { return store.IssuedCert{}, signErr })
	if !errors.Is(err, signErr) {
		t.Fatalf("RedeemJoinToken(sign error) err = %v, want signErr", err)
	}
	// The node row, agent cert, and consumption must all be rolled back.
	if _, err := s.NodeByName(ctx, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("node persisted despite sign failure: %v", err)
	}
	cons, err := s.ListJoinTokenConsumptions(ctx, store.ListJoinTokenConsumptionsParams{JoinTokenID: tok.ID, LimitCount: 10})
	if err != nil {
		t.Fatalf("ListJoinTokenConsumptions: %v", err)
	}
	if len(cons) != 0 {
		t.Errorf("consumptions = %d after rollback, want 0", len(cons))
	}
}
