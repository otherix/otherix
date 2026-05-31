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

	"github.com/otherix/otherix/internal/store"
)

func TestRedeemClusterJoinToken(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	hash := []byte("cluster-join-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindCluster,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(cluster): %v", err)
	}

	res, err := s.RedeemClusterJoinToken(ctx, store.RedeemClusterJoinTokenParams{TokenHash: hash})
	if err != nil {
		t.Fatalf("RedeemClusterJoinToken: %v", err)
	}
	if res.TokenID == uuid.Nil {
		t.Error("result TokenID is nil")
	}
	// The redemption records a consumption audit row (node id nil for a
	// cluster join).
	cons, err := s.ListJoinTokenConsumptions(ctx, store.ListJoinTokenConsumptionsParams{JoinTokenID: res.TokenID, LimitCount: 200})
	if err != nil || len(cons) != 1 {
		t.Fatalf("consumptions = (%v, %v), want 1", cons, err)
	}
	if cons[0].ConsumedByNodeID != nil {
		t.Errorf("ConsumedByNodeID = %v, want nil for a cluster join", cons[0].ConsumedByNodeID)
	}
}

func TestRedeemClusterJoinTokenRejections(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	// Unknown token.
	if _, err := s.RedeemClusterJoinToken(ctx, store.RedeemClusterJoinTokenParams{TokenHash: []byte("nope")}); !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("unknown token err = %v, want ErrJoinTokenInvalid", err)
	}

	// A node-kind token must not redeem at the cluster endpoint.
	nodeHash := []byte("node-kind-at-cluster")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: nodeHash,
		Kind:      store.JoinTokenKindNode,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(node): %v", err)
	}
	if _, err := s.RedeemClusterJoinToken(ctx, store.RedeemClusterJoinTokenParams{TokenHash: nodeHash}); !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("node token at cluster endpoint err = %v, want ErrJoinTokenInvalid", err)
	}

	// Expired cluster token.
	expHash := []byte("cluster-expired")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: expHash,
		Kind:      store.JoinTokenKindCluster,
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateJoinToken(expired): %v", err)
	}
	if _, err := s.RedeemClusterJoinToken(ctx, store.RedeemClusterJoinTokenParams{TokenHash: expHash}); !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("expired token err = %v, want ErrJoinTokenInvalid", err)
	}

	// max_uses exhaustion.
	one := int32(1)
	exhHash := []byte("cluster-exhausted")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: exhHash,
		Kind:      store.JoinTokenKindCluster,
		MaxUses:   &one,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(maxuses): %v", err)
	}
	if _, err := s.RedeemClusterJoinToken(ctx, store.RedeemClusterJoinTokenParams{TokenHash: exhHash}); err != nil {
		t.Fatalf("first cluster redeem: %v", err)
	}
	if _, err := s.RedeemClusterJoinToken(ctx, store.RedeemClusterJoinTokenParams{TokenHash: exhHash}); !errors.Is(err, store.ErrJoinTokenExhausted) {
		t.Errorf("exhausted token err = %v, want ErrJoinTokenExhausted", err)
	}
}
