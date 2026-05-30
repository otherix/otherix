// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestSetAndClearDefaultPoolName exercises the cluster_settings
// default-pool mutators backing PUT/DELETE /v1/cluster/default-pool.
// The singleton row is migration-seeded with a null default; the test
// restores that null on cleanup so the shared harness does not leak a
// default pool into other tests.
func TestSetAndClearDefaultPoolName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	t.Cleanup(func() { _ = s.ClearDefaultPoolName(context.Background()) })

	name := "pool-" + uuid.NewString()[:8]
	if err := s.SetDefaultPoolName(ctx, &name); err != nil {
		t.Fatalf("SetDefaultPoolName: %v", err)
	}
	got, err := s.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings: %v", err)
	}
	if got.DefaultPoolName == nil || *got.DefaultPoolName != name {
		t.Errorf("DefaultPoolName = %v, want %q", got.DefaultPoolName, name)
	}

	if err := s.ClearDefaultPoolName(ctx); err != nil {
		t.Fatalf("ClearDefaultPoolName: %v", err)
	}
	got, err = s.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings after clear: %v", err)
	}
	if got.DefaultPoolName != nil {
		t.Errorf("DefaultPoolName after clear = %q, want nil", *got.DefaultPoolName)
	}
}
