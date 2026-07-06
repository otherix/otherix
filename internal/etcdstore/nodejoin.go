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
// upsert the node, invoke the caller's sign callback for the resolved node, then
// persist the agent cert metadata + the consumption audit row in one
// transaction. The four domain rejections surface as store.ErrJoinTokenInvalid /
// ErrJoinTokenExhausted / ErrJoinNodeNameMismatch / ErrJoinNodeNameTaken; sign's
// own error propagates unwrapped with nothing persisted.
//
// Redemption is re-runnable. A node whose leaf was issued but never delivered
// (the join HTTP response was lost) has an UNCONFIRMED row - no heartbeat yet -
// and no operator-visible identity. Re-presenting the same token for the same
// name reuses that row, supersedes the stale undelivered cert, and issues a
// fresh one, WITHOUT consuming additional max_uses budget (the node is already
// counted). This is the recovery path for a lost bootstrap; a node that has ever
// heartbeated is confirmed-owned and cannot be re-enrolled via a token
// (ErrJoinNodeNameTaken). See upsertJoinNode for the confirm-signal gate.
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

	if err := s.precheckNodeMaxUses(ctx, token, p.NodeName); err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	node, err := s.upsertJoinNode(ctx, p, redeemNodeGateway(token))
	if err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	issued, err := sign(node)
	if err != nil {
		return store.RedeemJoinTokenResult{}, err
	}

	// Pre-build only the fixed-UUID cert PUT ops. The supersede-deletes, the count
	// decision, and the consumption write are recomputed per CAS attempt in
	// commitNodeRedemption - they depend on state a concurrent redemption can
	// change between attempts, so a snapshot taken here would be stale on retry.
	certPutOps, err := agentCertWriteOps(store.AgentCert{
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

	if err := s.commitNodeRedemption(ctx, token, node, p, certPutOps); err != nil {
		return store.RedeemJoinTokenResult{}, err
	}
	return store.RedeemJoinTokenResult{NodeID: node.ID, TokenID: token.ID}, nil
}

// hasConsumptionForNode reports whether a consumption record under tokenID already
// names nodeID as the consumer - i.e. this node has already been counted against
// the token (a prior redemption, possibly one whose leaf delivery was lost). A
// re-redemption for such a node re-issues without consuming additional max_uses
// budget.
func (s *Store) hasConsumptionForNode(ctx context.Context, tokenID, nodeID uuid.UUID) (bool, error) {
	items, err := s.c.Range(ctx, joinTokenConsumptionsIndexPrefix(tokenID))
	if err != nil {
		return false, err
	}
	for _, kv := range items {
		consID, err := uuid.Parse(string(kv.Value))
		if err != nil {
			return false, fmt.Errorf("parse consumption id from index: %v", err)
		}
		var c store.JoinTokenConsumption
		ok, err := s.c.GetJSON(ctx, joinTokenConsumptionKey(consID), &c)
		if err != nil {
			return false, err
		}
		if ok && c.ConsumedByNodeID != nil && *c.ConsumedByNodeID == nodeID {
			return true, nil
		}
	}
	return false, nil
}

// precheckNodeMaxUses does the advisory fast-path exhaustion reject BEFORE the
// expensive upsert + CSR sign, but ONLY for a genuinely new node. If the name
// already maps to a node with a consumption under this token, this is a re-issue
// (a retry after a lost leaf delivery) which must not reject as exhausted nor
// create an orphan - it returns nil and lets the CAS in commitNodeRedemption
// make the authoritative decision. An uncapped token (MaxUses nil) is a no-op.
func (s *Store) precheckNodeMaxUses(ctx context.Context, token store.JoinToken, nodeName string) error {
	if token.MaxUses == nil {
		return nil
	}
	if existing, lerr := s.NodeByName(ctx, nodeName); lerr == nil {
		counted, cerr := s.hasConsumptionForNode(ctx, token.ID, existing.ID)
		if cerr != nil {
			return cerr
		}
		if counted {
			return nil // re-issue: leave the reject to the authoritative CAS
		}
	} else if !errors.Is(lerr, store.ErrNotFound) {
		return fmt.Errorf("node lookup: %v", lerr)
	}
	cur, _, err := s.readNodeConsumedCount(ctx, joinTokenConsumedCountKey(token.ID), joinTokenConsumptionsIndexPrefix(token.ID))
	if err != nil {
		return err
	}
	if cur >= int64(*token.MaxUses) {
		return store.ErrJoinTokenExhausted
	}
	return nil
}

// commitNodeRedemption commits the new agent cert guarded by a compare-and-set
// on the token's consumed-count key, mirroring the cluster-join CAS loop. It is
// idempotent-safe across a re-redemption of a node that was already counted
// (a retry after a lost leaf delivery): each attempt re-derives, under the
// counter's revision guard,
//
//   - alreadyCounted: does a consumption row already name this node? If so the
//     redemption re-issues without consuming additional max_uses budget, and the
//     exhaustion check is bypassed - otherwise a single-use token could never be
//     re-redeemed to recover its own lost leaf.
//   - supersede ops: soft-revoke any cert currently on the node, recomputed fresh
//     so a retry after a concurrent winner revokes THAT winner's cert, leaving
//     exactly one active cert (ours).
//   - the count: cur+1 for a new node, unchanged for an already-counted one. The
//     count key is ALWAYS re-PUT even when unchanged, so its ModRevision stays the
//     per-token serialization point; two concurrent redemptions for the same node
//     conflict here and one retries, so they can never both commit two certs.
//
// A lost CAS means a concurrent redemption committed first - re-read and retry.
func (s *Store) commitNodeRedemption(ctx context.Context, token store.JoinToken, node store.Node, p store.RedeemJoinTokenParams, certPutOps []clientv3.Op) error {
	countKey := joinTokenConsumedCountKey(token.ID)
	for attempt := 0; attempt < joinRedeemCASRetries; attempt++ {
		cur, rev, err := s.readNodeConsumedCount(ctx, countKey, joinTokenConsumptionsIndexPrefix(token.ID))
		if err != nil {
			return err
		}
		// Re-evaluate per attempt: a concurrent redemption may have written this
		// node's consumption since the last read.
		alreadyCounted, err := s.hasConsumptionForNode(ctx, token.ID, node.ID)
		if err != nil {
			return err
		}
		if !alreadyCounted && token.MaxUses != nil && cur >= int64(*token.MaxUses) {
			return store.ErrJoinTokenExhausted
		}

		now := time.Now().UTC()
		supersedeOps, _, err := s.revokeNodeAgentCertsOps(ctx, node.ID, now, "superseded (re-enrollment)")
		if err != nil {
			return err
		}

		newCount := cur
		var consumptionOps []clientv3.Op
		if !alreadyCounted {
			newCount = cur + 1
			consumption := store.JoinTokenConsumption{
				ID:               uuid.New(),
				JoinTokenID:      token.ID,
				ConsumedByNodeID: &node.ID,
				ConsumedAt:       now,
				SourceIp:         p.SourceIP,
			}
			consVal, err := etcd.Marshal(consumption)
			if err != nil {
				return err
			}
			consumptionOps = []clientv3.Op{
				clientv3.OpPut(joinTokenConsumptionKey(consumption.ID), string(consVal)),
				clientv3.OpPut(joinTokenConsumptionsIndexKey(token.ID, consumption.ID), consumption.ID.String()),
			}
		}

		thenOps := []clientv3.Op{clientv3.OpPut(countKey, strconv.FormatInt(newCount, 10))}
		thenOps = append(thenOps, supersedeOps...)
		thenOps = append(thenOps, certPutOps...)
		thenOps = append(thenOps, consumptionOps...)

		guard := clientv3.Compare(clientv3.CreateRevision(countKey), "=", 0)
		if rev != 0 {
			guard = clientv3.Compare(clientv3.ModRevision(countKey), "=", rev)
		}
		txnResp, err := s.c.Raw().Txn(ctx).If(guard).Then(thenOps...).Commit()
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
// reused unless it has confirmed ownership by heartbeating
// (store.ErrJoinNodeNameTaken) or its gateway role differs from the role this
// token enrolls (store.ErrJoinNodeKindMismatch), otherwise a fresh pending node
// is created with the requested role. Reuse of an unconfirmed row is the lost-
// bootstrap recovery path (the caller supersedes the stale undelivered cert). A
// concurrent create that loses the name guard re-fetches the winner.
func (s *Store) upsertJoinNode(ctx context.Context, p store.RedeemJoinTokenParams, gatewayWanted bool) (store.Node, error) {
	existing, err := s.NodeByName(ctx, p.NodeName)
	if err == nil {
		// "Owned" is keyed on the CONFIRM signal (the node has authenticated with
		// its leaf and reported) - LastHeartbeatAt != nil - NOT on "a cert row
		// exists". A cert issued but never delivered (the join response was lost)
		// leaves an UNCONFIRMED node whose leaf nobody holds; re-enrollment reuses
		// the row and re-issues (the caller supersedes the stale cert). A node that
		// has ever heartbeated is owned and must not be re-enrolled via a token.
		if existing.LastHeartbeatAt != nil {
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
			// Same confirm gate as the reuse branch: a winner that has already
			// heartbeated is owned (a just-created concurrent winner is unconfirmed,
			// so this only bites a genuine race against a live node).
			if winner.LastHeartbeatAt != nil {
				return store.Node{}, store.ErrJoinNodeNameTaken
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
