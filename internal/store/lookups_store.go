// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"

	"github.com/google/uuid"
)

// Identifier-lookup methods on *Store back the API resolver
// (internal/api/handlers/internal/resolver), which turns an
// operator-supplied name or UUID into a row. They wrap the raw
// generated *Queries primitives and translate pgx.ErrNoRows into
// ErrNotFound so the resolver depends on the store sentinel, not pgx.
// As per-resource domain slices land they may move into the matching
// *_store.go file; they live together here while the resolver is the
// only consumer.

// NodeByName returns the node with the given name, or ErrNotFound.
func (s *Store) NodeByName(ctx context.Context, name string) (Node, error) {
	n, err := s.queries.GetNodeByName(ctx, name)
	if err != nil {
		return Node{}, translateNoRows(err)
	}
	return n, nil
}

// VMByName returns the vm with the given name, or ErrNotFound.
func (s *Store) VMByName(ctx context.Context, name string) (VM, error) {
	v, err := s.queries.GetVMByName(ctx, name)
	if err != nil {
		return VM{}, translateNoRows(err)
	}
	return v, nil
}

// TemplateByName returns the template with the given name, or
// ErrNotFound.
func (s *Store) TemplateByName(ctx context.Context, name string) (Template, error) {
	t, err := s.queries.GetTemplateByName(ctx, name)
	if err != nil {
		return Template{}, translateNoRows(err)
	}
	return t, nil
}

// StoragePoolByID returns the storage-pool instance with the given id,
// or ErrNotFound.
func (s *Store) StoragePoolByID(ctx context.Context, id uuid.UUID) (StoragePool, error) {
	p, err := s.queries.GetStoragePoolByID(ctx, id)
	if err != nil {
		return StoragePool{}, translateNoRows(err)
	}
	return p, nil
}

// StoragePoolsByName returns every storage-pool instance sharing the
// given name (the multi-instance model maps one name to potentially
// many per-node rows). An empty result is not an error.
func (s *Store) StoragePoolsByName(ctx context.Context, name string) ([]StoragePool, error) {
	return s.queries.ListStoragePoolsByName(ctx, name)
}

// ClusterSettings returns the cluster-settings singleton row, or
// ErrNotFound when it has not been initialised.
func (s *Store) ClusterSettings(ctx context.Context) (ClusterSetting, error) {
	c, err := s.queries.GetClusterSettings(ctx)
	if err != nil {
		return ClusterSetting{}, translateNoRows(err)
	}
	return c, nil
}

// SetDefaultPoolName writes the cluster-wide default pool name on the
// cluster_settings singleton. The caller validates that the name
// resolves to at least one existing pool instance before calling.
func (s *Store) SetDefaultPoolName(ctx context.Context, name *string) error {
	return s.queries.SetDefaultPoolName(ctx, name)
}

// ClearDefaultPoolName nulls the cluster-wide default pool name.
// Idempotent: clearing an already-null value is a no-op.
func (s *Store) ClearDefaultPoolName(ctx context.Context) error {
	return s.queries.ClearDefaultPoolName(ctx)
}
