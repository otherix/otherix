// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package artifactpools hosts the /v1/artifact-pools HTTP handlers - the
// cluster-level artifact-pool concept (slice B): a named content-addressed
// artifact store with a replication_factor and advisory membership. No per-node
// backing exists yet (sub-project C). Reads gated storage_pool:read, mutations
// storage_pool:manage.
package artifactpools

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the artifact-pool handlers depend on.
type Store interface {
	CreateArtifactPool(ctx context.Context, p store.CreateArtifactPoolParams) (store.ArtifactPool, error)
	ArtifactPoolByID(ctx context.Context, id uuid.UUID) (store.ArtifactPool, error)
	ArtifactPoolByName(ctx context.Context, name string) (store.ArtifactPool, error)
	ListArtifactPools(ctx context.Context, p store.ListArtifactPoolsParams) ([]store.ArtifactPool, error)
	DeleteArtifactPool(ctx context.Context, id uuid.UUID) error
}

// Handler bundles the dependencies for the artifact-pool routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler.
func New(s Store, log *slog.Logger) *Handler { return &Handler{store: s, log: log} }
