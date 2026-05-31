// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Idempotency keys back the mutating-endpoint replay guard. The client-supplied
// key is the primary key: /otherix/idempotency_keys/<key> -> JSON row. The
// middleware's in_flight -> completed lifecycle maps to compare-guarded writes
// (begin inserts only if absent; reclaim overwrites only an expired row).
// not-found / conflict / no-op cases return the store's not-found sentinel
// store.ErrNotFound, which the middleware tolerates.

func idempotencyKeyKey(key string) string { return etcd.Key("idempotency_keys") + "/" + key }

func idempotencyPrefix() string { return etcd.Key("idempotency_keys") + "/" }

// GetIdempotencyKey returns the row for key, or store.ErrNotFound.
func (s *Store) GetIdempotencyKey(ctx context.Context, key string) (store.IdempotencyKey, error) {
	var row store.IdempotencyKey
	found, err := s.c.GetJSON(ctx, idempotencyKeyKey(key), &row)
	if err != nil {
		return store.IdempotencyKey{}, err
	}
	if !found {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	return row, nil
}

// BeginIdempotencyKey inserts a fresh in_flight row, but only when the key is
// absent (the ON CONFLICT DO NOTHING analogue). A concurrent claim returns
// store.ErrNotFound so the middleware re-reads the winner's row.
func (s *Store) BeginIdempotencyKey(ctx context.Context, arg store.BeginIdempotencyKeyParams) (store.IdempotencyKey, error) {
	row := store.IdempotencyKey{
		Key:           arg.Key,
		UserID:        arg.UserID,
		RequestMethod: arg.RequestMethod,
		RequestPath:   arg.RequestPath,
		RequestHash:   arg.RequestHash,
		State:         "in_flight",
		CreatedAt:     time.Now().UTC(),
		ExpiresAt:     arg.ExpiresAt,
	}
	val, err := etcd.Marshal(row)
	if err != nil {
		return store.IdempotencyKey{}, err
	}
	k := idempotencyKeyKey(arg.Key)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(k), "=", 0)).
		Then(clientv3.OpPut(k, string(val))).
		Commit()
	if err != nil {
		return store.IdempotencyKey{}, fmt.Errorf("begin idempotency key txn: %v", err)
	}
	if !resp.Succeeded {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	return row, nil
}

// ReclaimIdempotencyKey overwrites an expired row in place (resetting it to
// in_flight). Returns store.ErrNotFound when the key is missing, not yet
// expired, or a concurrent caller reclaimed it first.
func (s *Store) ReclaimIdempotencyKey(ctx context.Context, arg store.ReclaimIdempotencyKeyParams) (store.IdempotencyKey, error) {
	k := idempotencyKeyKey(arg.Key)
	existing, modRev, found, err := s.idempotencyWithRev(ctx, arg.Key)
	if err != nil {
		return store.IdempotencyKey{}, err
	}
	now := time.Now().UTC()
	if !found || !existing.ExpiresAt.Before(now) {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	row := store.IdempotencyKey{
		Key:           arg.Key,
		UserID:        arg.UserID,
		RequestMethod: arg.RequestMethod,
		RequestPath:   arg.RequestPath,
		RequestHash:   arg.RequestHash,
		State:         "in_flight",
		CreatedAt:     now,
		ExpiresAt:     arg.ExpiresAt,
	}
	val, err := etcd.Marshal(row)
	if err != nil {
		return store.IdempotencyKey{}, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(k), "=", modRev)).
		Then(clientv3.OpPut(k, string(val))).
		Commit()
	if err != nil {
		return store.IdempotencyKey{}, fmt.Errorf("reclaim idempotency key txn: %v", err)
	}
	if !resp.Succeeded {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	return row, nil
}

// CompleteIdempotencyKey records the cached response and flips an in_flight row
// to completed. A missing or non-in_flight row is a no-op (matching the SQL
// state guard / :exec semantics).
func (s *Store) CompleteIdempotencyKey(ctx context.Context, arg store.CompleteIdempotencyKeyParams) error {
	row, err := s.GetIdempotencyKey(ctx, arg.Key)
	if err != nil {
		return nil //nolint:nilerr // missing row: nothing to complete (SQL no-op)
	}
	if row.State != "in_flight" {
		return nil
	}
	row.State = "completed"
	row.ResponseStatus = arg.ResponseStatus
	row.ResponseHeaders = arg.ResponseHeaders
	row.ResponseBody = arg.ResponseBody
	now := time.Now().UTC()
	row.CompletedAt = &now
	return s.c.PutJSON(ctx, idempotencyKeyKey(arg.Key), row)
}

// DeleteIdempotencyKey removes an in_flight row (the non-2xx cleanup path). A
// missing or already-completed row is a no-op (matching the SQL state guard).
func (s *Store) DeleteIdempotencyKey(ctx context.Context, key string) error {
	row, err := s.GetIdempotencyKey(ctx, key)
	if err != nil {
		return nil //nolint:nilerr // missing row: nothing to delete
	}
	if row.State != "in_flight" {
		return nil
	}
	if _, err := s.c.Delete(ctx, idempotencyKeyKey(key)); err != nil {
		return fmt.Errorf("delete idempotency key: %v", err)
	}
	return nil
}

// DeleteExpiredIdempotencyKeys removes every row past its expires_at, returning
// the number deleted. The cleanup hook the periodic sweep drives.
func (s *Store) DeleteExpiredIdempotencyKeys(ctx context.Context) (int64, error) {
	items, err := s.c.Range(ctx, idempotencyPrefix())
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var (
		ops     []clientv3.Op
		deleted int64
	)
	for _, kv := range items {
		var row store.IdempotencyKey
		if err := json.Unmarshal(kv.Value, &row); err != nil {
			return 0, fmt.Errorf("unmarshal idempotency key %q: %v", kv.Key, err)
		}
		if row.ExpiresAt.Before(now) {
			ops = append(ops, clientv3.OpDelete(kv.Key))
			deleted++
		}
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return 0, fmt.Errorf("delete expired idempotency keys: %v", err)
	}
	return deleted, nil
}

// idempotencyWithRev reads a row and its mod-revision (the reclaim CAS target).
func (s *Store) idempotencyWithRev(ctx context.Context, key string) (store.IdempotencyKey, int64, bool, error) {
	resp, err := s.c.Raw().Get(ctx, idempotencyKeyKey(key))
	if err != nil {
		return store.IdempotencyKey{}, 0, false, err
	}
	if len(resp.Kvs) == 0 {
		return store.IdempotencyKey{}, 0, false, nil
	}
	var row store.IdempotencyKey
	if err := json.Unmarshal(resp.Kvs[0].Value, &row); err != nil {
		return store.IdempotencyKey{}, 0, false, fmt.Errorf("unmarshal idempotency key %q: %v", key, err)
	}
	return row, resp.Kvs[0].ModRevision, true, nil
}
