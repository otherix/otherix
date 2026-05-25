// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cluster hosts the /v1/cluster/* HTTP handlers. The cluster
// surface is a thin facade over the cluster_settings singleton table —
// today only the default-pool reference (used by VM create when the
// request body omits `pool`), tomorrow potentially default-template,
// default-network, and similar cluster-shaping knobs. Reads are open to
// every authenticated role (cluster:read), mutations are admin-only
// (cluster:manage); see docs/rbac.md.
package cluster

import (
	"log/slog"

	"github.com/otherix/otherix/internal/store"
)

// Handler bundles the dependencies for the cluster routes.
type Handler struct {
	store *store.Store
	log   *slog.Logger
}

// New constructs a Handler.
func New(s *store.Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}
