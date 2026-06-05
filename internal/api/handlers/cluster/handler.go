// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cluster hosts the /v1/cluster/* HTTP handlers. The cluster
// surface is a thin facade over the cluster_settings singleton table —
// today only the default-pool reference (used by VM create when the
// request body omits `pool`), tomorrow potentially default-network and
// similar cluster-shaping knobs. Reads are open to
// every authenticated role (cluster:read), mutations are admin-only
// (cluster:manage); see docs/rbac.md.
package cluster

import (
	"context"
	"log/slog"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the cluster handlers depend on: the
// cluster-settings singleton accessors plus the pool-name lookup used
// to validate a default-pool reference. *etcdstore.Store satisfies it;
// depending on the interface rather than the concrete store narrows the
// handler's storage dependency to the methods it uses and lets tests
// substitute a fake.
type Store interface {
	ClusterSettings(ctx context.Context) (store.ClusterSetting, error)
	StoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error)
	SetDefaultPoolName(ctx context.Context, name *string) error
	ClearDefaultPoolName(ctx context.Context) error
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the cluster routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}
