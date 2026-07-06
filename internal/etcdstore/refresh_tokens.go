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

// Refresh tokens back the stateful, rotating half of the auth flow. Primary:
// /otherix/refresh_tokens/<id> -> JSON row. Indexes: by token-hash (the
// verification lookup), by family_id (the theft-detection cascade revoke), and
// by user_id (logout-from-all). Rotation revokes the parent and inserts the
// child in one transaction so a poll never sees a gap in the family chain.

func refreshTokenKey(id uuid.UUID) string { return etcd.Key("refresh_tokens", id.String()) }

func refreshTokenPrefix() string { return etcd.Key("refresh_tokens") + "/" }

func refreshTokenHashIndexKey(hash []byte) string {
	return etcd.Key("index", "refresh_tokens", "hash", hex.EncodeToString(hash))
}

func refreshTokenFamilyIndexKey(familyID, id uuid.UUID) string {
	return etcd.Key("index", "refresh_tokens", "family", familyID.String(), id.String())
}

func refreshTokenFamilyIndexPrefix(familyID uuid.UUID) string {
	return etcd.Key("index", "refresh_tokens", "family", familyID.String()) + "/"
}

func refreshTokenUserIndexKey(userID, id uuid.UUID) string {
	return etcd.Key("index", "refresh_tokens", "user", userID.String(), id.String())
}

func refreshTokenUserIndexPrefix(userID uuid.UUID) string {
	return etcd.Key("index", "refresh_tokens", "user", userID.String()) + "/"
}

// refreshTokenFromParams projects CreateRefreshTokenParams onto a fresh row,
// stamping issued_at.
func refreshTokenFromParams(arg store.CreateRefreshTokenParams, now time.Time) store.RefreshToken {
	return store.RefreshToken{
		ID:        arg.ID,
		UserID:    arg.UserID,
		TokenHash: arg.TokenHash,
		FamilyID:  arg.FamilyID,
		ParentID:  arg.ParentID,
		UserAgent: arg.UserAgent,
		IpAddress: arg.IpAddress,
		IssuedAt:  now,
		ExpiresAt: arg.ExpiresAt,
	}
}

// refreshTokenWriteOps returns the primary + hash/family/user index writes for a
// fresh refresh-token row.
func refreshTokenWriteOps(row store.RefreshToken, val string) []clientv3.Op {
	return []clientv3.Op{
		clientv3.OpPut(refreshTokenKey(row.ID), val),
		clientv3.OpPut(refreshTokenHashIndexKey(row.TokenHash), row.ID.String()),
		clientv3.OpPut(refreshTokenFamilyIndexKey(row.FamilyID, row.ID), row.ID.String()),
		clientv3.OpPut(refreshTokenUserIndexKey(row.UserID, row.ID), row.ID.String()),
	}
}

// CreateRefreshToken persists a fresh refresh-token row plus its hash / family /
// user indexes, returning the stored row.
func (s *Store) CreateRefreshToken(ctx context.Context, arg store.CreateRefreshTokenParams) (store.RefreshToken, error) {
	row := refreshTokenFromParams(arg, time.Now().UTC())
	val, err := etcd.Marshal(row)
	if err != nil {
		return store.RefreshToken{}, err
	}
	if _, err := s.c.Raw().Txn(ctx).Then(refreshTokenWriteOps(row, string(val))...).Commit(); err != nil {
		return store.RefreshToken{}, fmt.Errorf("create refresh token txn: %v", err)
	}
	return row, nil
}

// RefreshTokenByHash returns the row with the given token hash regardless of
// revocation state (the caller inspects revoked_at / expires_at), or
// store.ErrNotFound.
func (s *Store) RefreshTokenByHash(ctx context.Context, hash []byte) (store.RefreshToken, error) {
	id, found, err := s.resolveGuard(ctx, refreshTokenHashIndexKey(hash))
	if err != nil {
		return store.RefreshToken{}, err
	}
	if !found {
		return store.RefreshToken{}, store.ErrNotFound
	}
	var row store.RefreshToken
	ok, err := s.c.GetJSON(ctx, refreshTokenKey(id), &row)
	if err != nil {
		return store.RefreshToken{}, err
	}
	if !ok {
		return store.RefreshToken{}, store.ErrNotFound
	}
	return row, nil
}

// RevokeRefreshToken stamps revoked_at on a single token. Idempotent: an
// already-revoked or missing token is a no-op.
func (s *Store) RevokeRefreshToken(ctx context.Context, id uuid.UUID) error {
	var row store.RefreshToken
	found, err := s.c.GetJSON(ctx, refreshTokenKey(id), &row)
	if err != nil {
		return err
	}
	if !found || row.RevokedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	row.RevokedAt = &now
	return s.c.PutJSON(ctx, refreshTokenKey(id), row)
}

// RevokeRefreshTokenFamily revokes every active token in the family. This is the
// theft-detection cascade fired when a revoked token is replayed.
func (s *Store) RevokeRefreshTokenFamily(ctx context.Context, familyID uuid.UUID) error {
	return s.revokeIndexed(ctx, refreshTokenFamilyIndexPrefix(familyID))
}

// RevokeAllUserRefreshTokens revokes every active token for the user
// (logout-from-all).
func (s *Store) RevokeAllUserRefreshTokens(ctx context.Context, userID uuid.UUID) error {
	return s.revokeIndexed(ctx, refreshTokenUserIndexPrefix(userID))
}

// revokeIndexed loads every token referenced by an index prefix and stamps
// revoked_at on the active ones, committing in chunks below the per-txn op limit.
func (s *Store) revokeIndexed(ctx context.Context, indexPrefix string) error {
	items, err := s.c.Range(ctx, indexPrefix)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var ops []clientv3.Op
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return fmt.Errorf("corrupt refresh token index %q: %v", kv.Key, perr)
		}
		var row store.RefreshToken
		found, gerr := s.c.GetJSON(ctx, refreshTokenKey(id), &row)
		if gerr != nil {
			return gerr
		}
		if !found || row.RevokedAt != nil {
			continue
		}
		row.RevokedAt = &now
		val, merr := etcd.Marshal(row)
		if merr != nil {
			return merr
		}
		ops = append(ops, clientv3.OpPut(refreshTokenKey(id), string(val)))
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return fmt.Errorf("revoke refresh tokens: %v", err)
	}
	return nil
}

// TouchRefreshToken stamps last_used_at. A missing token is a no-op. The stamp
// commits under a ModRevision CAS so it can never clobber a concurrent revoke:
// a blind put would re-persist its stale revoked_at=nil snapshot and silently
// un-revoke a token the theft-detection cascade just burned.
func (s *Store) TouchRefreshToken(ctx context.Context, id uuid.UUID) error {
	now := time.Now().UTC()
	_, err := s.casRefreshTokenUpdate(ctx, id, func(row *store.RefreshToken) {
		row.LastUsedAt = &now
	})
	if errors.Is(err, store.ErrNotFound) {
		// Missing token: no-op, matching the prior contract.
		return nil
	}
	return err
}

// casRefreshTokenUpdate re-reads a refresh-token row, applies mutate, and commits
// under a ModRevision compare-and-set, retrying on a lost race so a concurrent
// writer (notably a family-burn / logout-all revoke) is never clobbered. A blind
// put that read the row before the revoke committed would re-persist
// revoked_at=nil and silently un-burn the token; the CAS-retry re-reads on
// conflict so revoked_at survives. mutate must be a pure field update on the
// passed row; it runs once per attempt on the freshest read. Returns
// store.ErrConcurrentUpdate after the retry bound is exhausted, and
// store.ErrNotFound for a missing token.
func (s *Store) casRefreshTokenUpdate(ctx context.Context, id uuid.UUID, mutate func(*store.RefreshToken)) (store.RefreshToken, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, err := s.c.Raw().Get(ctx, refreshTokenKey(id))
		if err != nil {
			return store.RefreshToken{}, err
		}
		if len(resp.Kvs) == 0 {
			return store.RefreshToken{}, store.ErrNotFound
		}
		modRev := resp.Kvs[0].ModRevision
		var row store.RefreshToken
		if err := json.Unmarshal(resp.Kvs[0].Value, &row); err != nil {
			return store.RefreshToken{}, fmt.Errorf("unmarshal refresh token %s: %v", id, err)
		}
		mutate(&row)
		val, err := etcd.Marshal(row)
		if err != nil {
			return store.RefreshToken{}, err
		}
		resp2, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(refreshTokenKey(id)), "=", modRev)).
			Then(clientv3.OpPut(refreshTokenKey(id), string(val))).
			Commit()
		if err != nil {
			return store.RefreshToken{}, fmt.Errorf("cas refresh token txn: %v", err)
		}
		if resp2.Succeeded {
			return row, nil
		}
	}
	return store.RefreshToken{}, store.ErrConcurrentUpdate
}

// RotateRefreshToken atomically revokes the parent token and inserts the child
// in one transaction gated by a CAS on the parent's ModRevision, so two
// rotations of the same parent can never both commit. Returns the child row.
// A missing parent returns store.ErrNotFound. An already-revoked parent, or a
// parent modified between the read and the commit (a concurrent rotation),
// returns store.ErrRefreshTokenConflict without inserting the child - the
// caller treats the double-spend as theft.
func (s *Store) RotateRefreshToken(ctx context.Context, parentID uuid.UUID, child store.CreateRefreshTokenParams) (store.RefreshToken, error) {
	resp, err := s.c.Raw().Get(ctx, refreshTokenKey(parentID))
	if err != nil {
		return store.RefreshToken{}, err
	}
	if len(resp.Kvs) == 0 {
		return store.RefreshToken{}, store.ErrNotFound
	}
	rev := resp.Kvs[0].ModRevision
	var parent store.RefreshToken
	if err := json.Unmarshal(resp.Kvs[0].Value, &parent); err != nil {
		return store.RefreshToken{}, err
	}
	if parent.RevokedAt != nil {
		return store.RefreshToken{}, store.ErrRefreshTokenConflict
	}

	now := time.Now().UTC()
	parent.RevokedAt = &now
	parentVal, err := etcd.Marshal(parent)
	if err != nil {
		return store.RefreshToken{}, err
	}
	childRow := refreshTokenFromParams(child, now)
	childVal, err := etcd.Marshal(childRow)
	if err != nil {
		return store.RefreshToken{}, err
	}
	ops := append(
		[]clientv3.Op{clientv3.OpPut(refreshTokenKey(parentID), string(parentVal))},
		refreshTokenWriteOps(childRow, string(childVal))...,
	)

	txnResp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(refreshTokenKey(parentID)), "=", rev)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.RefreshToken{}, fmt.Errorf("rotate refresh token txn: %v", err)
	}
	if !txnResp.Succeeded {
		// The parent moved between our read and the commit: a concurrent
		// rotation won. Nothing was written.
		return store.RefreshToken{}, store.ErrRefreshTokenConflict
	}
	return childRow, nil
}

// DeleteExpiredRefreshTokens removes every token past cutoff, dropping its hash /
// family / user indexes too. Returns the number deleted.
func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context, cutoff time.Time) (int64, error) {
	items, err := s.c.Range(ctx, refreshTokenPrefix())
	if err != nil {
		return 0, err
	}
	var (
		ops     []clientv3.Op
		deleted int64
	)
	for _, kv := range items {
		var row store.RefreshToken
		if err := json.Unmarshal(kv.Value, &row); err != nil {
			return 0, fmt.Errorf("unmarshal refresh token %q: %v", kv.Key, err)
		}
		if !row.ExpiresAt.Before(cutoff) {
			continue
		}
		ops = append(ops,
			clientv3.OpDelete(refreshTokenKey(row.ID)),
			clientv3.OpDelete(refreshTokenHashIndexKey(row.TokenHash)),
			clientv3.OpDelete(refreshTokenFamilyIndexKey(row.FamilyID, row.ID)),
			clientv3.OpDelete(refreshTokenUserIndexKey(row.UserID, row.ID)),
		)
		deleted++
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return 0, fmt.Errorf("delete expired refresh tokens: %v", err)
	}
	return deleted, nil
}
