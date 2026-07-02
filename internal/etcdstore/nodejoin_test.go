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

	nodejoinhandlers "github.com/otherix/otherix/internal/api/handlers/nodejoin"
	"github.com/otherix/otherix/internal/etcd"
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

func TestRedeemGatewayTokenPersistsIngressAdvertisedEndpoint(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	hash := []byte("gw-redeem-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID:        uuid.New(),
		TokenHash: hash,
		Kind:      store.JoinTokenKindGateway,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken(gateway): %v", err)
	}
	const wantIngress = "https://gw-1.example:9444"
	p := redeemParams(hash, "gw-1")
	p.IngressAdvertisedEndpoint = wantIngress
	res, err := s.RedeemJoinToken(ctx, p, func(store.Node) (store.IssuedCert, error) {
		return issuedCert(), nil
	})
	if err != nil {
		t.Fatalf("RedeemJoinToken(gateway): %v", err)
	}
	got, err := s.NodeByID(ctx, res.NodeID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if !got.HasRole(store.NodeRoleGateway) {
		t.Errorf("redeemed node role = %v, want gateway", got.Roles())
	}
	if got.IngressAdvertisedEndpoint != wantIngress {
		t.Errorf("IngressAdvertisedEndpoint = %q, want %q", got.IngressAdvertisedEndpoint, wantIngress)
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

// TestRedeemJoinTokenConcurrentMaxUses pins the atomic max_uses invariant: N
// concurrent redemptions of one max_uses=2 token must yield exactly 2
// successes; the rest must observe store.ErrJoinTokenExhausted. Against the
// pre-CAS read-then-write redemption all N pass the count check and commit.
func TestRedeemJoinTokenConcurrentMaxUses(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	two := int32(2)
	hash := []byte("concurrent-cap-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: hash, MaxUses: &two, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.RedeemJoinToken(ctx, redeemParams(hash, fmt.Sprintf("cap-node-%d", i)), func(store.Node) (store.IssuedCert, error) {
				return issuedCert(), nil
			})
		}()
	}
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrJoinTokenExhausted):
		default:
			t.Errorf("RedeemJoinToken(goroutine %d) = %v, want nil or store.ErrJoinTokenExhausted", i, err)
		}
	}
	if succeeded != 2 {
		t.Errorf("concurrent redemptions succeeded = %d, want exactly 2 (max_uses=2)", succeeded)
	}
}

// TestRedeemJoinTokenConcurrentIntendedNodeSingleUse pins the single-use
// intended-node invariant: two concurrent redemptions for the bound name must
// yield exactly one success and exactly one active cert for that node. The
// loser surfaces ErrJoinTokenExhausted (lost the consumed-count CAS) or
// ErrJoinNodeNameTaken (saw the winner's cert before its own count read).
func TestRedeemJoinTokenConcurrentIntendedNodeSingleUse(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	one := int32(1)
	bound := "bound-concurrent-node"
	hash := []byte("concurrent-bound-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: hash, IntendedNodeName: &bound, MaxUses: &one, ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	const n = 2
	errs := make([]error, n)
	results := make([]store.RedeemJoinTokenResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.RedeemJoinToken(ctx, redeemParams(hash, bound), func(store.Node) (store.IssuedCert, error) {
				return issuedCert(), nil
			})
		}()
	}
	wg.Wait()

	succeeded := 0
	var nodeID uuid.UUID
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
			nodeID = results[i].NodeID
		case errors.Is(err, store.ErrJoinTokenExhausted), errors.Is(err, store.ErrJoinNodeNameTaken):
		default:
			t.Errorf("RedeemJoinToken(goroutine %d) = %v, want nil, store.ErrJoinTokenExhausted, or store.ErrJoinNodeNameTaken", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent bound redemptions succeeded = %d, want exactly 1 (max_uses=1)", succeeded)
	}
	certs, err := cli.Range(ctx, etcd.Key("index", "agent_certs", "node", nodeID.String())+"/")
	if err != nil {
		t.Fatalf("range agent cert node index: %v", err)
	}
	if len(certs) != 1 {
		t.Errorf("active certs for node %s = %d, want exactly 1", bound, len(certs))
	}
}

// TestRedeemJoinTokenCountsPreCounterConsumptions pins the migration fallback:
// a token redeemed by a pre-counter CP build has a consumption index entry but
// no consumed-count key. The count read must fall back to the historical index
// count so max_uses still accounts for those redemptions.
func TestRedeemJoinTokenCountsPreCounterConsumptions(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	one := int32(1)
	hash := []byte("pre-counter-hash")
	jt, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: hash, MaxUses: &one, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	// Simulate the pre-counter redemption: index entry present, no counter key.
	consID := uuid.New()
	if err := cli.Put(ctx, etcd.Key("index", "join_token_consumptions", "token", jt.ID.String(), consID.String()), []byte(consID.String())); err != nil {
		t.Fatalf("seed pre-counter consumption index: %v", err)
	}

	if _, err := s.RedeemJoinToken(ctx, redeemParams(hash, "pre-counter-node"), func(store.Node) (store.IssuedCert, error) {
		return issuedCert(), nil
	}); !errors.Is(err, store.ErrJoinTokenExhausted) {
		t.Errorf("RedeemJoinToken(pre-counter exhausted token) = %v, want store.ErrJoinTokenExhausted", err)
	}
}

// TestRedeemJoinTokenSeedsCounterFromPreCounterIndex pins the counter-seeding
// transition of the migration fallback: a max_uses=2 token with one pre-counter
// consumption index entry and no counter key. The first post-fix redemption
// must succeed (the fallback reads count=1 from the index and commits the
// counter at 2), and the second must be rejected exhausted off the now
// authoritative counter - the historical index count is absorbed into the
// counter on first redemption.
func TestRedeemJoinTokenSeedsCounterFromPreCounterIndex(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	two := int32(2)
	hash := []byte("pre-counter-seed-hash")
	jt, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{ID: uuid.New(), TokenHash: hash, MaxUses: &two, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	// Simulate one pre-counter redemption: index entry present, no counter key.
	consID := uuid.New()
	if err := cli.Put(ctx, etcd.Key("index", "join_token_consumptions", "token", jt.ID.String(), consID.String()), []byte(consID.String())); err != nil {
		t.Fatalf("seed pre-counter consumption index: %v", err)
	}

	// First redemption succeeds: fallback reads count=1 from the index,
	// commits the counter at 2.
	if _, err := s.RedeemJoinToken(ctx, redeemParams(hash, "seed-node-1"), func(store.Node) (store.IssuedCert, error) {
		return issuedCert(), nil
	}); err != nil {
		t.Fatalf("RedeemJoinToken(first post-fix redemption) = %v, want nil", err)
	}

	// Second redemption is rejected: the authoritative counter now reads 2,
	// which meets max_uses=2.
	if _, err := s.RedeemJoinToken(ctx, redeemParams(hash, "seed-node-2"), func(store.Node) (store.IssuedCert, error) {
		return issuedCert(), nil
	}); !errors.Is(err, store.ErrJoinTokenExhausted) {
		t.Errorf("RedeemJoinToken(second post-fix redemption) = %v, want store.ErrJoinTokenExhausted", err)
	}
}
