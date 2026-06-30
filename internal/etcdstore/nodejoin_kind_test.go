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

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// preCreateNode materializes a pending, cert-less node row of the given kind on
// the shared name, the normal pre-join state an admin leaves behind after
// POST /v1/nodes. It returns the row id.
func preCreateNode(t *testing.T, s *etcdstore.Store, name, kind string) uuid.UUID {
	t.Helper()
	np := nodeParams(name)
	np.Kind = kind
	if _, err := s.CreateNode(context.Background(), np); err != nil {
		t.Fatalf("CreateNode(%s, %s): %v", name, kind, err)
	}
	return np.ID
}

// TestRedeemRejectsGatewayTokenAgainstNodeRow pins the kind invariant: a node's
// kind is fixed by the token that first claims the name. An admin pre-creates a
// node-kind row, then a gateway token redeems the same name. The redemption must
// be rejected (a kind-flip reuse would mis-issue a node-<name> leaf to a
// gateway), no cert may be signed, and the pre-existing row must keep its kind.
func TestRedeemRejectsGatewayTokenAgainstNodeRow(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	name := uniqueNodeName("preexist-node")
	preCreateNode(t, s, name, store.NodeKindNode)

	hash := []byte("kind-mismatch-gw-" + uuid.NewString())
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindGateway,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(gateway): %v", err)
	}

	signed := false
	_, err := s.RedeemJoinToken(ctx, redeemParams(hash, name), func(store.Node) (store.IssuedCert, error) {
		signed = true
		return issuedCert(), nil
	})
	if !errors.Is(err, store.ErrJoinNodeKindMismatch) {
		t.Fatalf("RedeemJoinToken err = %v, want ErrJoinNodeKindMismatch", err)
	}
	if signed {
		t.Error("sign callback ran, want no cert issued for a kind-mismatched reuse")
	}

	node, err := s.NodeByName(ctx, name)
	if err != nil {
		t.Fatalf("NodeByName(%s): %v", name, err)
	}
	if node.Kind != store.NodeKindNode {
		t.Errorf("node Kind = %q, want %q (kind must not flip)", node.Kind, store.NodeKindNode)
	}
	has, err := s.NodeHasActiveCert(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeHasActiveCert: %v", err)
	}
	if has {
		t.Error("node has an active cert, want none after a rejected redemption")
	}
}

// TestRedeemRejectsNodeTokenAgainstGatewayRow is the symmetric guard: a gateway
// pre-exists and a node token redeems the same name. The redemption is rejected
// and no cert is signed.
func TestRedeemRejectsNodeTokenAgainstGatewayRow(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	name := uniqueNodeName("preexist-gw")
	preCreateNode(t, s, name, store.NodeKindGateway)

	hash := []byte("kind-mismatch-node-" + uuid.NewString())
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindNode,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(node): %v", err)
	}

	signed := false
	_, err := s.RedeemJoinToken(ctx, redeemParams(hash, name), func(store.Node) (store.IssuedCert, error) {
		signed = true
		return issuedCert(), nil
	})
	if !errors.Is(err, store.ErrJoinNodeKindMismatch) {
		t.Fatalf("RedeemJoinToken err = %v, want ErrJoinNodeKindMismatch", err)
	}
	if signed {
		t.Error("sign callback ran, want no cert issued for a kind-mismatched reuse")
	}

	node, err := s.NodeByName(ctx, name)
	if err != nil {
		t.Fatalf("NodeByName(%s): %v", name, err)
	}
	if node.Kind != store.NodeKindGateway {
		t.Errorf("node Kind = %q, want %q (kind must not flip)", node.Kind, store.NodeKindGateway)
	}
}

// TestRedeemReusesSameKindNodeRow is the non-regression guard for the normal
// re-bootstrap path: a node token redeeming a pre-existing, cert-less node-kind
// row must still succeed and reuse the row.
func TestRedeemReusesSameKindNodeRow(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	name := uniqueNodeName("rebootstrap-node")
	wantID := preCreateNode(t, s, name, store.NodeKindNode)

	hash := []byte("same-kind-node-" + uuid.NewString())
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindNode,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(node): %v", err)
	}

	var signed store.Node
	res, err := s.RedeemJoinToken(ctx, redeemParams(hash, name), func(n store.Node) (store.IssuedCert, error) {
		signed = n
		return issuedCert(), nil
	})
	if err != nil {
		t.Fatalf("RedeemJoinToken(same-kind node): %v", err)
	}
	if res.NodeID != wantID {
		t.Errorf("reused node id = %v, want %v", res.NodeID, wantID)
	}
	if signed.Kind != store.NodeKindNode {
		t.Errorf("signed node Kind = %q, want %q", signed.Kind, store.NodeKindNode)
	}
}

// TestRedeemReusesSameKindGatewayRow is the symmetric non-regression guard: a
// gateway token redeeming a pre-existing, cert-less gateway-kind row succeeds
// and reuses the row.
func TestRedeemReusesSameKindGatewayRow(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	name := uniqueNodeName("rebootstrap-gw")
	wantID := preCreateNode(t, s, name, store.NodeKindGateway)

	hash := []byte("same-kind-gw-" + uuid.NewString())
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindGateway,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(gateway): %v", err)
	}

	var signed store.Node
	res, err := s.RedeemJoinToken(ctx, redeemParams(hash, name), func(n store.Node) (store.IssuedCert, error) {
		signed = n
		return issuedCert(), nil
	})
	if err != nil {
		t.Fatalf("RedeemJoinToken(same-kind gateway): %v", err)
	}
	if res.NodeID != wantID {
		t.Errorf("reused node id = %v, want %v", res.NodeID, wantID)
	}
	if signed.Kind != store.NodeKindGateway {
		t.Errorf("signed node Kind = %q, want %q", signed.Kind, store.NodeKindGateway)
	}
}
