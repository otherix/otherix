// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Join tokens are addressed by UUID with a by-hash index (backing node-join
// redemption's plaintext->row lookup) and a per-token consumption audit index
// (backing the consumption_count correlated subquery and the consumption
// listing). RevokeJoinToken clamps expires_at to now; expiry, not deletion, is
// how a token is retired.

func joinTokenKey(id uuid.UUID) string { return etcd.Key("join_tokens", id.String()) }

func joinTokenPrefix() string { return etcd.Key("join_tokens") + "/" }

func joinTokenHashIndexKey(hash []byte) string {
	return etcd.Key("index", "join_tokens", "hash", hex.EncodeToString(hash))
}

func joinTokenConsumptionKey(id uuid.UUID) string {
	return etcd.Key("join_token_consumptions", id.String())
}

func joinTokenConsumptionsIndexKey(tokenID, id uuid.UUID) string {
	return etcd.Key("index", "join_token_consumptions", "token", tokenID.String(), id.String())
}

func joinTokenConsumptionsIndexPrefix(tokenID uuid.UUID) string {
	return etcd.Key("index", "join_token_consumptions", "token", tokenID.String()) + "/"
}

// JoinTokenByID returns the join token with the given id, or store.ErrNotFound.
func (s *Store) JoinTokenByID(ctx context.Context, id uuid.UUID) (store.JoinToken, error) {
	var jt store.JoinToken
	found, err := s.c.GetJSON(ctx, joinTokenKey(id), &jt)
	if err != nil {
		return store.JoinToken{}, err
	}
	if !found {
		return store.JoinToken{}, store.ErrNotFound
	}
	return jt, nil
}

// JoinTokenByHash returns the join token whose hash matches, or
// store.ErrNotFound. Used by node-join redemption to resolve the presented
// plaintext's hash to a row.
func (s *Store) JoinTokenByHash(ctx context.Context, hash []byte) (store.JoinToken, error) {
	id, found, err := s.resolveGuard(ctx, joinTokenHashIndexKey(hash))
	if err != nil {
		return store.JoinToken{}, err
	}
	if !found {
		return store.JoinToken{}, store.ErrNotFound
	}
	return s.JoinTokenByID(ctx, id)
}

// CreateJoinToken inserts a join token, stamping created_at and writing the
// primary + by-hash index atomically.
func (s *Store) CreateJoinToken(ctx context.Context, arg store.CreateJoinTokenParams) (store.JoinToken, error) {
	kind := arg.Kind
	if kind == "" {
		kind = store.JoinTokenKindNode
	}
	jt := store.JoinToken{
		ID:               arg.ID,
		TokenHash:        arg.TokenHash,
		Kind:             kind,
		IntendedNodeName: arg.IntendedNodeName,
		CreatedByUserID:  arg.CreatedByUserID,
		ExpiresAt:        arg.ExpiresAt,
		MaxUses:          arg.MaxUses,
		CreatedAt:        time.Now().UTC(),
	}
	val, err := etcd.Marshal(jt)
	if err != nil {
		return store.JoinToken{}, err
	}
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(joinTokenKey(jt.ID), string(val)),
			clientv3.OpPut(joinTokenHashIndexKey(jt.TokenHash), jt.ID.String()),
		).
		Commit(); err != nil {
		return store.JoinToken{}, fmt.Errorf("create join token txn: %v", err)
	}
	return jt, nil
}

// ListJoinTokens returns join tokens (each with its consumption count) ordered
// by (created_at, id) ascending, after the cursor, capped at LimitCount.
// Expired rows surface only when IncludeExpired is set.
func (s *Store) ListJoinTokens(ctx context.Context, arg store.ListJoinTokensParams) ([]store.ListJoinTokensRow, error) {
	items, err := s.c.Range(ctx, joinTokenPrefix())
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]store.ListJoinTokensRow, 0, len(items))
	for _, kv := range items {
		var jt store.JoinToken
		if err := json.Unmarshal(kv.Value, &jt); err != nil {
			return nil, fmt.Errorf("unmarshal join token %q: %v", kv.Key, err)
		}
		if !arg.IncludeExpired && !jt.ExpiresAt.After(now) {
			continue
		}
		if !afterCursor(jt.CreatedAt, jt.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		count, err := s.countPrefix(ctx, joinTokenConsumptionsIndexPrefix(jt.ID))
		if err != nil {
			return nil, err
		}
		kind := jt.Kind
		if kind == "" {
			kind = store.JoinTokenKindNode
		}
		out = append(out, store.ListJoinTokensRow{
			ID:               jt.ID,
			TokenHash:        jt.TokenHash,
			Kind:             kind,
			IntendedNodeName: jt.IntendedNodeName,
			CreatedByUserID:  jt.CreatedByUserID,
			ExpiresAt:        jt.ExpiresAt,
			MaxUses:          jt.MaxUses,
			CreatedAt:        jt.CreatedAt,
			ConsumptionCount: count,
		})
	}
	sortByCreatedAtID(out, func(r store.ListJoinTokensRow) (time.Time, uuid.UUID) { return r.CreatedAt, r.ID })
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// RevokeJoinToken clamps expires_at to now (LEAST(expires_at, now)). Idempotent:
// re-revoking an already-expired token leaves it unchanged. A missing token is
// silent (zero rows).
func (s *Store) RevokeJoinToken(ctx context.Context, id uuid.UUID) error {
	jt, err := s.JoinTokenByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	now := time.Now().UTC()
	if jt.ExpiresAt.Before(now) || jt.ExpiresAt.Equal(now) {
		return nil
	}
	jt.ExpiresAt = now
	return s.c.PutJSON(ctx, joinTokenKey(id), jt)
}

// ListJoinTokenConsumptions returns the consumption audit rows for a token,
// ordered by (consumed_at, id) ascending, after the cursor, capped at
// LimitCount.
func (s *Store) ListJoinTokenConsumptions(ctx context.Context, arg store.ListJoinTokenConsumptionsParams) ([]store.JoinTokenConsumption, error) {
	items, err := s.c.Range(ctx, joinTokenConsumptionsIndexPrefix(arg.JoinTokenID))
	if err != nil {
		return nil, err
	}
	out := make([]store.JoinTokenConsumption, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt consumption index %q: %v", kv.Key, perr)
		}
		var c store.JoinTokenConsumption
		found, gerr := s.c.GetJSON(ctx, joinTokenConsumptionKey(id), &c)
		if gerr != nil {
			return nil, gerr
		}
		if !found {
			continue
		}
		if !afterCursor(c.ConsumedAt, c.ID, arg.CursorConsumedAt, arg.CursorID) {
			continue
		}
		out = append(out, c)
	}
	sortByCreatedAtID(out, func(c store.JoinTokenConsumption) (time.Time, uuid.UUID) { return c.ConsumedAt, c.ID })
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}
