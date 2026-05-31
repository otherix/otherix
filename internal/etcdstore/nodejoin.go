// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// RedeemJoinToken runs the Step 2 redemption: resolve the token by hash
// (rejecting unknown/expired), enforce max_uses and the intended-node binding,
// upsert the node (rejecting a name that already holds an active cert), invoke
// the caller's sign callback for the resolved node, then persist the agent cert
// metadata + the consumption audit row in one transaction. The four domain
// rejections surface as store.ErrJoinTokenInvalid / ErrJoinTokenExhausted /
// ErrJoinNodeNameMismatch / ErrJoinNodeNameTaken; sign's own error propagates
// unwrapped with nothing persisted.
//
// Unlike the SQL path's SELECT FOR UPDATE, this is a read-then-write sequence:
// safe for the single-node default; the HA path will gate it behind the
// placement-style advisory lock (ROADMAP).
func (s *Store) RedeemJoinToken(ctx context.Context, p store.RedeemJoinTokenParams, sign func(node store.Node) (store.IssuedCert, error)) (store.RedeemJoinTokenResult, error) {
	token, err := s.JoinTokenByHash(ctx, p.TokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.RedeemJoinTokenResult{}, store.ErrJoinTokenInvalid
		}
		return store.RedeemJoinTokenResult{}, fmt.Errorf("token lookup: %v", err)
	}
	if err := s.validateRedeemToken(ctx, token, store.JoinTokenKindNode); err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	if token.IntendedNodeName != nil && *token.IntendedNodeName != p.NodeName {
		return store.RedeemJoinTokenResult{}, store.ErrJoinNodeNameMismatch
	}

	node, err := s.upsertJoinNode(ctx, p)
	if err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	issued, err := sign(node)
	if err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	certOps, err := agentCertWriteOps(store.AgentCert{
		ID:                uuid.New(),
		NodeID:            node.ID,
		Serial:            issued.Serial,
		FingerprintSha256: issued.FingerprintSha256,
		SubjectDn:         issued.SubjectDN,
		NotBefore:         issued.NotBefore,
		NotAfter:          issued.NotAfter,
		IssuedAt:          time.Now().UTC(),
	})
	if err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	consumption := store.JoinTokenConsumption{
		ID:               uuid.New(),
		JoinTokenID:      token.ID,
		ConsumedByNodeID: &node.ID,
		ConsumedAt:       time.Now().UTC(),
		SourceIp:         p.SourceIP,
	}
	consVal, err := etcd.Marshal(consumption)
	if err != nil {
		return store.RedeemJoinTokenResult{}, err
	}
	ops := certOps
	ops = append(ops,
		clientv3.OpPut(joinTokenConsumptionKey(consumption.ID), string(consVal)),
		clientv3.OpPut(joinTokenConsumptionsIndexKey(token.ID, consumption.ID), consumption.ID.String()),
	)
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return store.RedeemJoinTokenResult{}, fmt.Errorf("redeem join token txn: %v", err)
	}

	return store.RedeemJoinTokenResult{NodeID: node.ID, TokenID: token.ID}, nil
}

// validateRedeemTokenKind enforces the kind-and-expiry invariants shared by node
// and cluster joins: the token is unexpired and matches the expected kind (empty
// Kind reads as node for back-compat - so a node token cannot redeem at the
// cluster endpoint and vice versa).
func (s *Store) validateRedeemTokenKind(token store.JoinToken, wantKind string) error {
	if !token.ExpiresAt.After(time.Now().UTC()) {
		return store.ErrJoinTokenInvalid
	}
	kind := token.Kind
	if kind == "" {
		kind = store.JoinTokenKindNode
	}
	if kind != wantKind {
		return store.ErrJoinTokenInvalid
	}
	return nil
}

// validateRedeemToken adds the (non-atomic) max_uses pre-check to the kind
// invariants. The node-join path relies on it; max_uses enforcement here is a
// read-then-write and is not race-tight under concurrent redemptions (tracked
// for the HA advisory-lock work). The cluster-join path does NOT use this - it
// enforces max_uses atomically via a CAS counter, since its payload is the CA
// private key.
func (s *Store) validateRedeemToken(ctx context.Context, token store.JoinToken, wantKind string) error {
	if err := s.validateRedeemTokenKind(token, wantKind); err != nil {
		return err
	}
	if token.MaxUses != nil {
		count, err := s.countPrefix(ctx, joinTokenConsumptionsIndexPrefix(token.ID))
		if err != nil {
			return fmt.Errorf("count consumptions: %v", err)
		}
		if count >= int64(*token.MaxUses) {
			return store.ErrJoinTokenExhausted
		}
	}
	return nil
}

// upsertJoinNode resolves the node row for a redemption: an existing node is
// reused unless it still holds an active cert (store.ErrJoinNodeNameTaken),
// otherwise a fresh pending node is created. A concurrent create that loses the
// name guard re-fetches the winner.
func (s *Store) upsertJoinNode(ctx context.Context, p store.RedeemJoinTokenParams) (store.Node, error) {
	existing, err := s.NodeByName(ctx, p.NodeName)
	if err == nil {
		hasActive, err := s.NodeHasActiveCert(ctx, existing.ID)
		if err != nil {
			return store.Node{}, fmt.Errorf("check active cert: %v", err)
		}
		if hasActive {
			return store.Node{}, store.ErrJoinNodeNameTaken
		}
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Node{}, fmt.Errorf("node lookup: %v", err)
	}

	node, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    p.NodeName,
		Architecture:            p.Architecture,
		AdvertisedEndpoint:      p.AdvertisedEndpoint,
		MigrationHost:           p.MigrationHost,
		MigrationPortRangeStart: p.MigrationPortRangeStart,
		MigrationPortRangeEnd:   p.MigrationPortRangeEnd,
		Status:                  store.NodeStatusPending,
	})
	if err != nil {
		if errors.Is(err, store.ErrNodeNameExists) {
			return s.NodeByName(ctx, p.NodeName)
		}
		return store.Node{}, fmt.Errorf("create node: %v", err)
	}
	return node, nil
}
