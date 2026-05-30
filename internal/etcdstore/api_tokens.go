// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// API tokens are addressed by UUID with a by-user index (backing the per-user
// listing and the user-delete revocation cascade) and a by-hash index (backing
// the authn middleware's bearer lookup at cutover). There is no name guard -
// api token names are not unique.

func apiTokenKey(id uuid.UUID) string { return etcd.Key("api_tokens", id.String()) }

func apiTokenUserIndexKey(userID, id uuid.UUID) string {
	return etcd.Key("index", "api_tokens", "user", userID.String(), id.String())
}

func apiTokenUserIndexPrefix(userID uuid.UUID) string {
	return etcd.Key("index", "api_tokens", "user", userID.String()) + "/"
}

func apiTokenHashIndexKey(hash []byte) string {
	return etcd.Key("index", "api_tokens", "hash", hex.EncodeToString(hash))
}

// APITokenByID returns the api token with the given id, or store.ErrNotFound.
func (s *Store) APITokenByID(ctx context.Context, id uuid.UUID) (store.ApiToken, error) {
	var tok store.ApiToken
	found, err := s.c.GetJSON(ctx, apiTokenKey(id), &tok)
	if err != nil {
		return store.ApiToken{}, err
	}
	if !found {
		return store.ApiToken{}, store.ErrNotFound
	}
	return tok, nil
}

// CreateAPIToken inserts an api token, stamping created_at and writing the
// primary + by-user + by-hash indexes. The caller supplies the hash and prefix.
func (s *Store) CreateAPIToken(ctx context.Context, arg store.CreateApiTokenParams) (store.ApiToken, error) {
	tok := store.ApiToken{
		ID:        arg.ID,
		UserID:    arg.UserID,
		Name:      arg.Name,
		TokenHash: arg.TokenHash,
		Prefix:    arg.Prefix,
		ExpiresAt: arg.ExpiresAt,
		CreatedAt: time.Now().UTC(),
	}
	val, err := etcd.Marshal(tok)
	if err != nil {
		return store.ApiToken{}, err
	}
	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(apiTokenKey(tok.ID), string(val)),
			clientv3.OpPut(apiTokenUserIndexKey(tok.UserID, tok.ID), tok.ID.String()),
			clientv3.OpPut(apiTokenHashIndexKey(tok.TokenHash), tok.ID.String()),
		).
		Commit(); err != nil {
		return store.ApiToken{}, fmt.Errorf("create api token txn: %v", err)
	}
	return tok, nil
}

// ListAPITokensByUser returns the api tokens owned by a user, ordered by
// (created_at, id) ascending, after the cursor, capped at LimitCount. Revoked
// rows surface only when IncludeRevoked is set; expired-but-not-revoked rows
// always surface.
func (s *Store) ListAPITokensByUser(ctx context.Context, arg store.ListApiTokensByUserParams) ([]store.ApiToken, error) {
	items, err := s.c.Range(ctx, apiTokenUserIndexPrefix(arg.UserID))
	if err != nil {
		return nil, err
	}
	out := make([]store.ApiToken, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt api token user index %q: %v", kv.Key, perr)
		}
		tok, err := s.APITokenByID(ctx, id)
		if err != nil {
			continue
		}
		if tok.RevokedAt != nil && !arg.IncludeRevoked {
			continue
		}
		if !afterCursor(tok.CreatedAt, tok.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		out = append(out, tok)
	}
	sortByCreatedAtID(out, func(t store.ApiToken) (time.Time, uuid.UUID) { return t.CreatedAt, t.ID })
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// RevokeAPIToken stamps revoked_at on the token. Idempotent: re-revoking an
// already-revoked token is a no-op, and revoking a missing token is silent
// (matching the SQL update affecting zero rows).
func (s *Store) RevokeAPIToken(ctx context.Context, id uuid.UUID) error {
	tok, err := s.APITokenByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if tok.RevokedAt != nil {
		return nil
	}
	now := time.Now().UTC()
	tok.RevokedAt = &now
	return s.c.PutJSON(ctx, apiTokenKey(id), tok)
}
