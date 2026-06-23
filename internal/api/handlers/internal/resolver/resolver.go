// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package resolver translates an operator-supplied identifier into the
// underlying store row.
//
// Node and VM identifiers are name-only on
// paths/bodies/filters: they reject UUID format with CodeUUIDInName
// before any DB roundtrip. Storage pools remain polymorphic - under
// the multi-instance model UUID addressing is essential for
// per-node-instance operations (the same pool name can live on
// multiple nodes; only a UUID disambiguates).
//
// The resolver stays a handler-scoped package
// (internal/api/handlers/internal/resolver) and not a store-scoped
// helper: the wire-level "identifier" concept belongs to the API
// edge, the store layer stays a thin type binding.
package resolver

import (
	"context"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Kind enumerates the resource categories the resolver knows about.
// Carried in ResolutionError so callers can build typed error
// envelopes without re-deriving the category from the function name.
type Kind string

// Resource kinds the resolver recognises. The string values double as
// the `details.kind` payload on wire envelopes built by the handlers.
const (
	KindPool Kind = "storage_pool"
	KindNode Kind = "node"
	KindVM   Kind = "vm"
)

// Querier is the subset of *store.Store domain lookups the resolver
// needs. Name-only lookups for Node / VM; both UUID-by-id and
// name-by-list for Pool (multi-instance carve-out - UUID branch
// retained for per-instance addressing). Keeping the surface narrow
// lets unit tests pass a hand-rolled fake, and lets any store backend
// satisfy it structurally. Method names match the store's domain
// methods (no Get prefix); single-row lookups return store.ErrNotFound
// when the row is missing.
type Querier interface {
	StoragePoolByID(ctx context.Context, id uuid.UUID) (store.StoragePool, error)
	StoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error)
	ClusterSettings(ctx context.Context) (store.ClusterSetting, error)
	NodeByName(ctx context.Context, name string) (store.Node, error)
	VMByName(ctx context.Context, name string) (store.VM, error)
}
