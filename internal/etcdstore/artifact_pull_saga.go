// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// pullTokenPrefix is the plaintext prefix of a single-use blob-pull bearer token,
// mirroring the otx_-prefixed form of the other opaque tokens.
const pullTokenPrefix = "otx_pull_"

func pullSagaKey(id uuid.UUID) string { return etcd.Key("artifact_pull_saga", id.String()) }

// CreatePullSaga writes the saga record and mints an ephemeral per-op bearer
// token, returning the token plaintext exactly once. The token is NOT persisted
// CP-side: it is verified AGENT-SIDE by the holder's primed serve-token map
// (internal/agent/blobserve.go serveTokenStore), which binds the token to the
// blob digest and is bounded by the serve listener lifecycle. C1 keeps no
// CP-side single-use record (a CP-side single-use/lease is a possible C2
// hardening); only the saga record is stored.
func (s *Store) CreatePullSaga(ctx context.Context, p store.CreatePullSagaParams) (store.ArtifactPullSaga, string, error) {
	saga := store.ArtifactPullSaga{
		ID:           p.ID,
		Digest:       p.Digest,
		ConsumerNode: p.ConsumerNode,
		HolderNode:   p.HolderNode,
		Phase:        store.PullSagaPhasePending,
		CreatedAt:    time.Now().UTC(),
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return store.ArtifactPullSaga{}, "", fmt.Errorf("mint pull token: %v", err)
	}
	plaintext := pullTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	if err := s.c.PutJSON(ctx, pullSagaKey(saga.ID), saga); err != nil {
		return store.ArtifactPullSaga{}, "", fmt.Errorf("create pull saga: %v", err)
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
