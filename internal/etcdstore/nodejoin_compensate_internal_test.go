// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/store"
)

// TestRedeemJoinTokenLeavesNodeOnNonExhaustionCommitError proves the compensating
// delete fires ONLY on ErrJoinTokenExhausted - the sole error class returned
// before commitNodeRedemption runs its Txn().Commit(), so the fresh row is
// provably orphaned. A transport-style commit error may accompany a raft proposal
// that commits LATE (after the client observed the error), so deleting then could
// destroy a node whose redemption actually succeeded. On such errors the fresh row
// must be left intact (recoverable, reusable by the retry) rather than
// destructively removed.
func TestRedeemJoinTokenLeavesNodeOnNonExhaustionCommitError(t *testing.T) {
	s, _ := FreshStore(t)
	ctx := context.Background()

	hash := []byte("compensate-nonexhaust-hash")
	if _, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID: uuid.New(), TokenHash: hash, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	// Force a non-exhaustion (transport-style) commit error AFTER the node row is
	// created. errors.Is(_, ErrJoinTokenExhausted) is false, so the compensating
	// delete must NOT fire.
	orig := commitNodeRedemptionFn
	t.Cleanup(func() { commitNodeRedemptionFn = orig })
	wantErr := errors.New("etcdserver: request timed out")
	commitNodeRedemptionFn = func(*Store, context.Context, store.JoinToken, store.Node, store.RedeemJoinTokenParams, []clientv3.Op) error {
		return wantErr
	}

	params := store.RedeemJoinTokenParams{
		TokenHash:               hash,
		NodeName:                "agent-timeout",
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://agent-timeout:9443",
		MigrationHost:           "10.0.0.9",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
	}
	_, err := s.RedeemJoinToken(ctx, params, func(store.Node) (store.IssuedCert, error) {
		now := time.Now().UTC()
		return store.IssuedCert{
			Serial:            []byte("serial"),
			FingerprintSha256: []byte("fp-" + uuid.NewString()),
			SubjectDN:         "CN=node",
			NotBefore:         now,
			NotAfter:          now.Add(time.Hour),
		}, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RedeemJoinToken error = %v, want the forced commit error %v", err, wantErr)
	}

	// The fresh row this call created must SURVIVE the non-exhaustion error. With
	// the over-broad `if created` trigger it would have been compensating-deleted
	// (this lookup would then miss), destroying a node whose redemption may have
	// committed late server-side.
	if _, err := s.NodeByName(ctx, "agent-timeout"); err != nil {
		t.Errorf("NodeByName after non-exhaustion commit error = %v, want the row to survive", err)
	}
}
