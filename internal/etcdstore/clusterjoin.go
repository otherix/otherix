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

// clusterRedeemCASRetries bounds the compare-and-set retries when concurrent
// cluster-join redemptions race on the same token's consumption counter.
const clusterRedeemCASRetries = 16

// RedeemClusterJoinToken runs the cluster-replica redemption: resolve the token
// by hash (rejecting unknown/expired/non-cluster), enforce max_uses, and record
// the consumption audit row. Unlike node join there is no CSR or node upsert -
// the caller (the /v1/cluster/join handler) returns the cluster CA cert + key
// from the active CA row so the joining replica can sign its own peer cert.
//
// The unknown / expired / wrong-kind cases all collapse to ErrJoinTokenInvalid
// so the endpoint never reveals which failed; an over-cap token surfaces
// ErrJoinTokenExhausted.
//
// Like RedeemJoinToken this is a read-then-write sequence (single-node safe; HA
// will gate it behind the placement-style advisory lock).
func (s *Store) RedeemClusterJoinToken(ctx context.Context, p store.RedeemClusterJoinTokenParams) (store.RedeemClusterJoinResult, error) {
	token, err := s.JoinTokenByHash(ctx, p.TokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.RedeemClusterJoinResult{}, store.ErrJoinTokenInvalid
		}
		return store.RedeemClusterJoinResult{}, fmt.Errorf("token lookup: %v", err)
	}
	if err := s.validateRedeemTokenKind(token, store.JoinTokenKindCluster); err != nil {
		return store.RedeemClusterJoinResult{}, err
	}

	// max_uses is enforced atomically: read the consumption counter, reject when
	// the cap is reached, then write the increment + the consumption guarded by a
	// compare on the counter's revision. A lost CAS means a concurrent redemption
	// committed first - re-read and re-check. This keeps a max_uses=1 cluster
	// token (which yields the CA private key) from being redeemed twice under a
	// concurrent-POST race.
	countKey := joinTokenConsumedCountKey(token.ID)
	for attempt := 0; attempt < clusterRedeemCASRetries; attempt++ {
		cur, rev, err := s.readConsumedCount(ctx, countKey)
		if err != nil {
			return store.RedeemClusterJoinResult{}, err
		}
		if token.MaxUses != nil && cur >= int64(*token.MaxUses) {
			return store.RedeemClusterJoinResult{}, store.ErrJoinTokenExhausted
		}

		consumption := store.JoinTokenConsumption{
			ID:          uuid.New(),
			JoinTokenID: token.ID,
			ConsumedAt:  time.Now().UTC(),
			SourceIp:    p.SourceIP,
		}
		consVal, err := etcd.Marshal(consumption)
		if err != nil {
			return store.RedeemClusterJoinResult{}, err
		}

		guard := clientv3.Compare(clientv3.CreateRevision(countKey), "=", 0)
		if rev != 0 {
			guard = clientv3.Compare(clientv3.ModRevision(countKey), "=", rev)
		}
		txnResp, err := s.c.Raw().Txn(ctx).
			If(guard).
			Then(
				clientv3.OpPut(countKey, strconv.FormatInt(cur+1, 10)),
				clientv3.OpPut(joinTokenConsumptionKey(consumption.ID), string(consVal)),
				clientv3.OpPut(joinTokenConsumptionsIndexKey(token.ID, consumption.ID), consumption.ID.String()),
			).
			Commit()
		if err != nil {
			return store.RedeemClusterJoinResult{}, fmt.Errorf("redeem cluster join token txn: %v", err)
		}
		if txnResp.Succeeded {
			return store.RedeemClusterJoinResult{TokenID: token.ID}, nil
		}
		// CAS lost to a concurrent redemption; re-read and retry.
	}
	return store.RedeemClusterJoinResult{}, fmt.Errorf("redeem cluster join token: too many concurrent attempts")
}

// readConsumedCount returns the token's current redemption count and the
// counter key's ModRevision (0 when the key is absent, so the caller guards the
// first write on CreateRevision==0).
func (s *Store) readConsumedCount(ctx context.Context, countKey string) (count, modRev int64, err error) {
	resp, err := s.c.Raw().Get(ctx, countKey)
	if err != nil {
		return 0, 0, fmt.Errorf("read consumed count: %v", err)
	}
	if len(resp.Kvs) == 0 {
		return 0, 0, nil
	}
	kv := resp.Kvs[0]
	n, perr := strconv.ParseInt(string(kv.Value), 10, 64)
	if perr != nil {
		return 0, 0, fmt.Errorf("corrupt consumed count %q: %v", kv.Value, perr)
	}
	return n, kv.ModRevision, nil
}
