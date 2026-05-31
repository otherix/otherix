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

	nodejoinhandlers "github.com/otherix/otherix/internal/api/handlers/nodejoin"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the node-join handler contract (ActiveCACert +
// RedeemJoinToken).
var _ nodejoinhandlers.Store = (*etcdstore.Store)(nil)

func redeemParams(hash []byte, nodeName string) store.RedeemJoinTokenParams {
	return store.RedeemJoinTokenParams{
		TokenHash:               hash,
		NodeName:                nodeName,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + nodeName + ":9443",
		MigrationHost:           "10.0.0.9",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
	}
}

func issuedCert() store.IssuedCert {
	now := time.Now().UTC()
	return store.IssuedCert{
		Serial:            []byte("serial"),
		FingerprintSha256: []byte("fp-" + uuid.NewString()),
		SubjectDN:         "CN=node",
		NotBefore:         now,
		NotAfter:          now.Add(8760 * time.Hour),
	}
}

func TestRedeemJoinTokenCreatesNodeAndCert(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	hash := []byte("redeem-hash-1")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: hash, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	var signedNode store.Node
	res, err := s.RedeemJoinToken(ctx, redeemParams(hash, "agent-a"), func(n store.Node) (store.IssuedCert, error) {
		signedNode = n
		return issuedCert(), nil
	})
	if err != nil {
		t.Fatalf("RedeemJoinToken: %v", err)
	}
	if res.NodeID == uuid.Nil || signedNode.Name != "agent-a" {
		t.Errorf("redeem result = %+v, signed node = %+v", res, signedNode)
	}
	// Node now exists and holds an active cert.
	if _, err := s.NodeByName(ctx, "agent-a"); err != nil {
		t.Errorf("NodeByName after redeem: %v", err)
	}
	has, err := s.NodeHasActiveCert(ctx, res.NodeID)
	if err != nil || !has {
		t.Errorf("NodeHasActiveCert = (%v, %v), want true", has, err)
	}
	// The consumption audit row is recorded.
	cons, err := s.ListJoinTokenConsumptions(ctx, store.ListJoinTokenConsumptionsParams{JoinTokenID: res.TokenID, LimitCount: 200})
	if err != nil || len(cons) != 1 {
		t.Errorf("consumptions = (%v, %v), want 1", cons, err)
	}
}

func TestRedeemJoinTokenRejectsClusterKind(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	hash := []byte("cluster-kind-hash")
	row, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindCluster,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJoinToken(cluster): %v", err)
	}
	// Kind must round-trip through the store.
	if row.Kind != store.JoinTokenKindCluster {
		t.Errorf("created Kind = %q, want cluster", row.Kind)
	}
	// A cluster token must not redeem for a node leaf at /v1/nodes/join.
	if _, err := s.RedeemJoinToken(ctx, redeemParams(hash, "agent-x"), func(store.Node) (store.IssuedCert, error) {
		return issuedCert(), nil
	}); !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("RedeemJoinToken(cluster kind) err = %v, want ErrJoinTokenInvalid", err)
	}
}

func TestRedeemJoinTokenRejections(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	// Unknown token.
	if _, err := s.RedeemJoinToken(ctx, redeemParams([]byte("nope"), "x"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("unknown token = %v, want store.ErrJoinTokenInvalid", err)
	}

	// Expired token.
	expHash := []byte("expired-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: expHash, ExpiresAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateJoinToken(expired): %v", err)
	}
	if _, err := s.RedeemJoinToken(ctx, redeemParams(expHash, "x"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); !errors.Is(err, store.ErrJoinTokenInvalid) {
		t.Errorf("expired token = %v, want store.ErrJoinTokenInvalid", err)
	}

	// Max-uses exhausted (max_uses=1, one redemption already done).
	one := int32(1)
	exhHash := []byte("exhaust-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: exhHash, MaxUses: &one, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateJoinToken(maxuses): %v", err)
	}
	if _, err := s.RedeemJoinToken(ctx, redeemParams(exhHash, "agent-b"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); err != nil {
		t.Fatalf("first redeem of capped token: %v", err)
	}
	if _, err := s.RedeemJoinToken(ctx, redeemParams(exhHash, "agent-c"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); !errors.Is(err, store.ErrJoinTokenExhausted) {
		t.Errorf("exhausted token = %v, want store.ErrJoinTokenExhausted", err)
	}

	// Intended-node binding mismatch.
	bound := "bound-node"
	bindHash := []byte("bind-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: bindHash, IntendedNodeName: &bound, MaxUses: &one, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateJoinToken(bound): %v", err)
	}
	if _, err := s.RedeemJoinToken(ctx, redeemParams(bindHash, "other-node"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); !errors.Is(err, store.ErrJoinNodeNameMismatch) {
		t.Errorf("binding mismatch = %v, want store.ErrJoinNodeNameMismatch", err)
	}

	// Node name already holds an active cert.
	takenHash1 := []byte("taken-1")
	takenHash2 := []byte("taken-2")
	for _, h := range [][]byte{takenHash1, takenHash2} {
		if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: h, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
			t.Fatalf("CreateJoinToken(taken): %v", err)
		}
	}
	if _, err := s.RedeemJoinToken(ctx, redeemParams(takenHash1, "dup-node"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); err != nil {
		t.Fatalf("first redeem of dup-node: %v", err)
	}
	if _, err := s.RedeemJoinToken(ctx, redeemParams(takenHash2, "dup-node"), func(store.Node) (store.IssuedCert, error) { return issuedCert(), nil }); !errors.Is(err, store.ErrJoinNodeNameTaken) {
		t.Errorf("node name taken = %v, want store.ErrJoinNodeNameTaken", err)
	}

	// sign error propagates with nothing persisted.
	signHash := []byte("sign-err-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: signHash, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateJoinToken(sign): %v", err)
	}
	sentinel := errors.New("sign failed")
	if _, err := s.RedeemJoinToken(ctx, redeemParams(signHash, "sign-node"), func(store.Node) (store.IssuedCert, error) { return store.IssuedCert{}, sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("sign error = %v, want propagated sentinel", err)
	}
}
