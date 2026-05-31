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
	if err := s.validateRedeemToken(ctx, token, store.JoinTokenKindCluster); err != nil {
		return store.RedeemClusterJoinResult{}, err
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
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(joinTokenConsumptionKey(consumption.ID), string(consVal)),
			clientv3.OpPut(joinTokenConsumptionsIndexKey(token.ID, consumption.ID), consumption.ID.String()),
		).
		Commit(); err != nil {
		return store.RedeemClusterJoinResult{}, fmt.Errorf("redeem cluster join token txn: %v", err)
	}

	return store.RedeemClusterJoinResult{TokenID: token.ID}, nil
}
