// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
// max_uses is enforced atomically, mirroring cluster-join: the commit is
// guarded by a compare-and-set on the token's consumed-count key, so concurrent
// redemptions cannot push a token past max_uses and a single-use intended-node
// token cannot yield two certs.
func (s *Store) RedeemJoinToken(ctx context.Context, p store.RedeemJoinTokenParams, sign func(node store.Node) (store.IssuedCert, error)) (store.RedeemJoinTokenResult, error) {
	token, err := s.JoinTokenByHash(ctx, p.TokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.RedeemJoinTokenResult{}, store.ErrJoinTokenInvalid
		}
		return store.RedeemJoinTokenResult{}, fmt.Errorf("token lookup: %v", err)
	}
	// The node-join endpoint serves both hypervisor nodes and self-registering
	// ingress gateways; a cluster token (CA private key) must never redeem here.
	if err := s.validateRedeemTokenKind(token, store.JoinTokenKindNode, store.JoinTokenKindGateway); err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	if token.IntendedNodeName != nil && *token.IntendedNodeName != p.NodeName {
		return store.RedeemJoinTokenResult{}, store.ErrJoinNodeNameMismatch
	}

	// Fast-path: reject an already-exhausted token before doing the expensive
	// upsert + CSR sign. This is advisory only - the authoritative cap
	// enforcement is the compare-and-set in commitNodeRedemption, which
	// re-reads the count under the guard and handles the token becoming
	// exhausted concurrently after this check.
	if token.MaxUses != nil {
		cur, _, err := s.readNodeConsumedCount(ctx, joinTokenConsumedCountKey(token.ID), joinTokenConsumptionsIndexPrefix(token.ID))
		if err != nil {
			return store.RedeemJoinTokenResult{}, err
		}
		if cur >= int64(*token.MaxUses) {
			return store.RedeemJoinTokenResult{}, store.ErrJoinTokenExhausted
		}
	}

	node, err := s.upsertJoinNode(ctx, p, redeemNodeGateway(token))
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

	if err := s.commitNodeRedemption(ctx, token, ops); err != nil {
		return store.RedeemJoinTokenResult{}, err
	}
	return store.RedeemJoinTokenResult{NodeID: node.ID, TokenID: token.ID}, nil
}

// commitNodeRedemption commits the cert + consumption ops guarded by a
// compare-and-set on the token's consumed-count key, mirroring the cluster-join
// CAS loop: read the consumption counter, reject with ErrJoinTokenExhausted
// when the max_uses cap is reached, then write the increment + ops guarded by a
// compare on the counter's revision. A lost CAS means a concurrent redemption
// committed first - re-read and re-check. This keeps a max_uses=N token from
// enrolling more than N nodes and a single-use intended-node token from
// yielding two certs under a concurrent-POST race.
func (s *Store) commitNodeRedemption(ctx context.Context, token store.JoinToken, ops []clientv3.Op) error {
	countKey := joinTokenConsumedCountKey(token.ID)
	for attempt := 0; attempt < joinRedeemCASRetries; attempt++ {
		cur, rev, err := s.readNodeConsumedCount(ctx, countKey, joinTokenConsumptionsIndexPrefix(token.ID))
		if err != nil {
			return err
		}
		if token.MaxUses != nil && cur >= int64(*token.MaxUses) {
			return store.ErrJoinTokenExhausted
		}

		guard := clientv3.Compare(clientv3.CreateRevision(countKey), "=", 0)
		if rev != 0 {
			guard = clientv3.Compare(clientv3.ModRevision(countKey), "=", rev)
		}
		txnResp, err := s.c.Raw().Txn(ctx).
			If(guard).
			Then(append([]clientv3.Op{clientv3.OpPut(countKey, strconv.FormatInt(cur+1, 10))}, ops...)...).
			Commit()
		if err != nil {
			return fmt.Errorf("redeem join token txn: %v", err)
		}
		if txnResp.Succeeded {
			return nil
		}
		// CAS lost to a concurrent redemption; re-read and retry.
	}
	return errors.New("redeem join token: too many concurrent attempts")
}

// readNodeConsumedCount returns the token's redemption count and the counter
// key's ModRevision. When the counter key is absent (a token first redeemed
// before the counter existed, or never redeemed), it falls back to the
// historical consumption-index count so max_uses still accounts for
// pre-counter redemptions; rev stays 0 so the first writer guards on
// CreateRevision==0.
func (s *Store) readNodeConsumedCount(ctx context.Context, countKey, indexPrefix string) (count, modRev int64, err error) {
	n, rev, err := s.readConsumedCount(ctx, countKey)
	if err != nil {
		return 0, 0, err
	}
	if rev != 0 {
		return n, rev, nil
	}
	idx, err := s.countPrefix(ctx, indexPrefix)
	if err != nil {
		return 0, 0, fmt.Errorf("count consumptions: %v", err)
	}
	return idx, 0, nil
}

// validateRedeemTokenKind enforces the kind-and-expiry invariants shared by node
// and cluster joins: the token is unexpired and matches one of the kinds the
// endpoint accepts (empty Kind reads as node for back-compat). The node-join
// endpoint accepts node + gateway; the cluster-join endpoint accepts only
// cluster - so a node token cannot redeem at the cluster endpoint and vice versa.
func (s *Store) validateRedeemTokenKind(token store.JoinToken, wantKinds ...string) error {
	if !token.ExpiresAt.After(time.Now().UTC()) {
		return store.ErrJoinTokenInvalid
	}
	kind := token.Kind
	if kind == "" {
		kind = store.JoinTokenKindNode
	}
	for _, want := range wantKinds {
		if kind == want {
			return nil
		}
	}
	return store.ErrJoinTokenInvalid
}

// redeemNodeGateway reports whether a redeemed join token materializes an
// ingress-gateway node: a gateway token assigns the gateway role, every other
// token a hypervisor node.
func redeemNodeGateway(token store.JoinToken) bool {
	return token.Kind == store.JoinTokenKindGateway
}

// upsertJoinNode resolves the node row for a redemption: an existing node is
// reused unless it still holds an active cert (store.ErrJoinNodeNameTaken) or its
// gateway role differs from the role this token enrolls
// (store.ErrJoinNodeKindMismatch), otherwise a fresh pending node is created with
// the requested role. A concurrent create that loses the name guard re-fetches
// the winner.
func (s *Store) upsertJoinNode(ctx context.Context, p store.RedeemJoinTokenParams, gatewayWanted bool) (store.Node, error) {
	existing, err := s.NodeByName(ctx, p.NodeName)
	if err == nil {
		hasActive, err := s.NodeHasActiveCert(ctx, existing.ID)
		if err != nil {
			return store.Node{}, fmt.Errorf("check active cert: %v", err)
		}
		if hasActive {
			return store.Node{}, store.ErrJoinNodeNameTaken
		}
		// A node's role is fixed by the token that first claims the name. The
		// caller selects the CSR signing template from the reused row's role, so a
		// token of a different role reusing this row would mis-issue an identity
		// (e.g. a gateway token yielding a node-<name> leaf). Reject rather than
		// silently re-stamping the row.
		if existing.HasRole(store.NodeRoleGateway) != gatewayWanted {
			return store.Node{}, store.ErrJoinNodeKindMismatch
		}
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Node{}, fmt.Errorf("node lookup: %v", err)
	}

	node, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                        uuid.New(),
		Name:                      p.NodeName,
		Gateway:                   gatewayWanted,
		Architecture:              p.Architecture,
		AdvertisedEndpoint:        p.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: p.IngressAdvertisedEndpoint,
		MigrationHost:             p.MigrationHost,
		MigrationPortRangeStart:   p.MigrationPortRangeStart,
		MigrationPortRangeEnd:     p.MigrationPortRangeEnd,
		Status:                    store.NodeStatusPending,
	})
	if err != nil {
		if errors.Is(err, store.ErrNodeNameExists) {
			// Lost a concurrent create for this name. Reuse the winner's row, but
			// hold the same invariant as the reuse branch above: a winner of a
			// different role must not yield a mis-issued identity.
			winner, ferr := s.NodeByName(ctx, p.NodeName)
			if ferr != nil {
				return store.Node{}, ferr
			}
			if winner.HasRole(store.NodeRoleGateway) != gatewayWanted {
				return store.Node{}, store.ErrJoinNodeKindMismatch
			}
			return winner, nil
		}
		return store.Node{}, fmt.Errorf("create node: %v", err)
	}
	return node, nil
}
