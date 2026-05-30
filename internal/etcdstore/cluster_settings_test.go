// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
)

// ClusterSettings is one of the six resolver.Querier lookups; the full
// cluster.Store and resolver-embedding assertions land once storage-pools,
// nodes, templates, and vms are implemented.

func TestClusterSettingsDefaultsWhenAbsent(t *testing.T) {
	s, _ := startStore(t)
	got, err := s.ClusterSettings(context.Background())
	if err != nil {
		t.Fatalf("ClusterSettings(fresh): %v", err)
	}
	if got.ID != 1 {
		t.Errorf("ClusterSettings().ID = %d, want 1", got.ID)
	}
	if got.DefaultPoolName != nil {
		t.Errorf("ClusterSettings().DefaultPoolName = %v, want nil", *got.DefaultPoolName)
	}
}

func TestClusterSettingsSetAndClearDefaultPool(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := "pool-a"
	if err := s.SetDefaultPoolName(ctx, &name); err != nil {
		t.Fatalf("SetDefaultPoolName: %v", err)
	}
	got, err := s.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings after set: %v", err)
	}
	if got.DefaultPoolName == nil || *got.DefaultPoolName != name {
		t.Errorf("DefaultPoolName = %v, want %q", got.DefaultPoolName, name)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("updated_at %v before created_at %v", got.UpdatedAt, got.CreatedAt)
	}

	if err := s.ClearDefaultPoolName(ctx); err != nil {
		t.Fatalf("ClearDefaultPoolName: %v", err)
	}
	cleared, err := s.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings after clear: %v", err)
	}
	if cleared.DefaultPoolName != nil {
		t.Errorf("DefaultPoolName after clear = %v, want nil", *cleared.DefaultPoolName)
	}
}
