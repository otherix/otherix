// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// ErrPullTokenInvalid is returned by ConsumePullToken when the presented token
// is unknown, already consumed (replay), expired, or its {digest, consumer}
// binding does not match. One opaque error for every reject case so the holder
// listener carries no oracle.
var ErrPullTokenInvalid = errors.New("etcdstore: pull token invalid, consumed, expired, or mis-bound")

// pullTokenPrefix is the plaintext prefix of a single-use blob-pull bearer token,
// mirroring the otx_-prefixed form of the other opaque tokens.
const pullTokenPrefix = "otx_pull_"

func pullSagaKey(id uuid.UUID) string { return etcd.Key("artifact_pull_saga", id.String()) }

// pullTokenKey maps to artifact_pull_token/<sha256hex(plaintext)> -> JSON token
// record. The hash is the key, mirroring the sha256 storage form of every
// opaque token (auth.HashToken); the plaintext is returned exactly once at mint
// and never stored. Single-use: ConsumePullToken deletes the key inside the
// verify txn.
func pullTokenKey(hash []byte) string {
	return etcd.Key("artifact_pull_token", hex.EncodeToString(hash))
}

// pullTokenRecord is the stored token binding: the {digest, consumer} it is
// valid for and its absolute expiry. Single-use is enforced by deleting the key.
type pullTokenRecord struct {
	Digest       string    `json:"digest"`
	ConsumerNode uuid.UUID `json:"consumer_node"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// CreatePullSaga writes the saga record and mints a single-use, short-TTL bearer
// token bound to {digest, consumer}, returning the token plaintext exactly once.
// The token is stored hashed (sha256, auth.HashToken) so a store read never
// yields a usable token. Both keys commit in one txn.
func (s *Store) CreatePullSaga(ctx context.Context, p store.CreatePullSagaParams) (store.ArtifactPullSaga, string, error) {
	now := time.Now().UTC()
	saga := store.ArtifactPullSaga{
		ID:           p.ID,
		Digest:       p.Digest,
		ConsumerNode: p.ConsumerNode,
		HolderNode:   p.HolderNode,
		Phase:        store.PullSagaPhasePending,
		CreatedAt:    now,
	}
	sagaVal, err := etcd.Marshal(saga)
	if err != nil {
		return store.ArtifactPullSaga{}, "", err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return store.ArtifactPullSaga{}, "", fmt.Errorf("mint pull token: %v", err)
	}
	plaintext := pullTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	rec := pullTokenRecord{Digest: p.Digest, ConsumerNode: p.ConsumerNode, ExpiresAt: now.Add(p.TokenTTL)}
	recVal, err := etcd.Marshal(rec)
	if err != nil {
		return store.ArtifactPullSaga{}, "", err
	}

	if _, err := s.c.Raw().Txn(ctx).Then(
		clientv3.OpPut(pullSagaKey(saga.ID), string(sagaVal)),
		clientv3.OpPut(pullTokenKey(auth.HashToken(plaintext)), string(recVal)),
	).Commit(); err != nil {
		return store.ArtifactPullSaga{}, "", fmt.Errorf("create pull saga txn: %v", err)
	}
	return saga, plaintext, nil
}

// PullSagaByID returns the saga record, or store.ErrNotFound.
func (s *Store) PullSagaByID(ctx context.Context, id uuid.UUID) (store.ArtifactPullSaga, error) {
	var saga store.ArtifactPullSaga
	found, err := s.c.GetJSON(ctx, pullSagaKey(id), &saga)
	if err != nil {
		return store.ArtifactPullSaga{}, err
	}
	if !found {
		return store.ArtifactPullSaga{}, store.ErrNotFound
	}
	return saga, nil
}

// UpdatePullSagaServeEndpoint records the holder's serve endpoint and advances
// the phase to serving. Best-effort metadata; not gated. This is a
// read-modify-write (last-writer-wins) that assumes a single writer per saga (the
// C1 broker mutates a saga sequentially from one goroutine); concurrent mutators
// would need a ModRevision CAS.
func (s *Store) UpdatePullSagaServeEndpoint(ctx context.Context, id uuid.UUID, endpoint string) error {
	saga, err := s.PullSagaByID(ctx, id)
	if err != nil {
		return err
	}
	saga.ServeEndpoint = endpoint
	saga.Phase = store.PullSagaPhaseServing
	return s.c.PutJSON(ctx, pullSagaKey(id), saga)
}

// SetPullSagaPhase advances the saga phase (pulling / complete / failed). This is
// a read-modify-write (last-writer-wins) that assumes a single writer per saga
// (the C1 broker mutates a saga sequentially from one goroutine); concurrent
// mutators would need a ModRevision CAS.
func (s *Store) SetPullSagaPhase(ctx context.Context, id uuid.UUID, phase store.PullSagaPhase) error {
	saga, err := s.PullSagaByID(ctx, id)
	if err != nil {
		return err
	}
	saga.Phase = phase
	return s.c.PutJSON(ctx, pullSagaKey(id), saga)
}

// ConsumePullToken verifies plaintext against the stored binding and consumes it
// (single-use): it deletes the token key inside a CAS txn so a replay races to
// nothing. Returns ErrPullTokenInvalid for an unknown / already-consumed /
// expired token or a {digest, consumer} mismatch - one opaque error, no oracle.
func (s *Store) ConsumePullToken(ctx context.Context, plaintext, digest string, consumer uuid.UUID) error {
	key := pullTokenKey(auth.HashToken(plaintext))
	var rec pullTokenRecord
	found, err := s.c.GetJSON(ctx, key, &rec)
	if err != nil {
		return err
	}
	if !found {
		return ErrPullTokenInvalid
	}
	if rec.Digest != digest || rec.ConsumerNode != consumer || time.Now().UTC().After(rec.ExpiresAt) {
		return ErrPullTokenInvalid
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "!=", 0)).
		Then(clientv3.OpDelete(key)).
		Commit()
	if err != nil {
		return fmt.Errorf("consume pull token txn: %v", err)
	}
	if !resp.Succeeded {
		return ErrPullTokenInvalid
	}
	return nil
}
