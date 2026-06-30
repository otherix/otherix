// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestRedeemGatewayJoinTokenCreatesGatewayNode pins the gateway self-registration
// path: a kind=gateway token redeems at the node-join endpoint and creates a node
// row stamped Kind=gateway with the advertised endpoint persisted (HIGH-2: a later
// CP path nudges the gateway by its AdvertisedEndpoint, so an empty one silently
// drops it to the heartbeat backstop). The cert binding is recorded just like a node.
func TestRedeemGatewayJoinTokenCreatesGatewayNode(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	hash := []byte("gateway-redeem-hash")
	row, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindGateway,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJoinToken(gateway): %v", err)
	}
	if row.Kind != store.JoinTokenKindGateway {
		t.Fatalf("created Kind = %q, want gateway", row.Kind)
	}

	params := redeemParams(hash, "edge1")
	var signedNode store.Node
	res, err := s.RedeemJoinToken(ctx, params, func(n store.Node) (store.IssuedCert, error) {
		signedNode = n
		return issuedCert(), nil
	})
	if err != nil {
		t.Fatalf("RedeemJoinToken(gateway): %v", err)
	}

	// The sign callback sees a gateway-kind node, so the handler can dispatch
	// to the gateway CSR signer.
	if signedNode.Kind != store.NodeKindGateway {
		t.Errorf("signed node Kind = %q, want gateway", signedNode.Kind)
	}

	node, err := s.NodeByName(ctx, "edge1")
	if err != nil {
		t.Fatalf("NodeByName(edge1): %v", err)
	}
	if node.Kind != store.NodeKindGateway {
		t.Errorf("node Kind = %q, want gateway", node.Kind)
	}
	if node.AdvertisedEndpoint == "" {
		t.Error("node AdvertisedEndpoint is empty, want the redeemed endpoint persisted")
	}
	if node.AdvertisedEndpoint != params.AdvertisedEndpoint {
		t.Errorf("node AdvertisedEndpoint = %q, want %q", node.AdvertisedEndpoint, params.AdvertisedEndpoint)
	}

	// The fingerprint binding is recorded in agent_certs.
	has, err := s.NodeHasActiveCert(ctx, res.NodeID)
	if err != nil || !has {
		t.Errorf("NodeHasActiveCert = (%v, %v), want true", has, err)
	}
}
