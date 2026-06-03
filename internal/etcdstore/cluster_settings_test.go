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

func TestSeedVNIRangeFirstWriterWins(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if err := s.SeedVNIRange(ctx, 2000, 3000); err != nil {
		t.Fatalf("SeedVNIRange first call: %v", err)
	}
	if err := s.SeedVNIRange(ctx, 4000, 5000); err != nil {
		t.Fatalf("SeedVNIRange second call: %v", err)
	}
	min, max, err := s.VNIRange(ctx)
	if err != nil {
		t.Fatalf("VNIRange: %v", err)
	}
	if min != 2000 || max != 3000 {
		t.Errorf("VNIRange() = (%d,%d), want (2000,3000)", min, max)
	}
}

func TestSeedVNIRangeSecondCallWithBadConfigShortCircuits(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if err := s.SeedVNIRange(ctx, 1000, 2000); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	// A replica booting with an INVALID local config must NOT fail - the value is
	// already seeded and immutable.
	if err := s.SeedVNIRange(ctx, 5000, 100); err != nil { // min>max: invalid local config
		t.Fatalf("second seed with bad local config = %v, want nil (already seeded)", err)
	}
	min, max, err := s.VNIRange(ctx)
	if err != nil {
		t.Fatalf("VNIRange: %v", err)
	}
	if min != 1000 || max != 2000 {
		t.Errorf("range = [%d,%d], want the originally seeded [1000,2000]", min, max)
	}
}

func TestVNIRangeDefaultWhenUnset(t *testing.T) {
	s, _ := startStore(t)
	min, max, err := s.VNIRange(context.Background())
	if err != nil {
		t.Fatalf("VNIRange: %v", err)
	}
	if min != 1000 || max != 65535 {
		t.Errorf("VNIRange() default = (%d,%d), want (1000,65535)", min, max)
	}
}

func TestSeedVNIRangeRejectsBadBounds(t *testing.T) {
	cases := []struct {
		name     string
		min, max int
	}{
		{"min below floor", 500, 600},
		{"min equals max", 2000, 2000},
		{"max above vxlan ceiling", 1000, 16777216},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := startStore(t)
			if err := s.SeedVNIRange(context.Background(), tc.min, tc.max); err == nil {
				t.Errorf("SeedVNIRange(%d,%d) = nil, want error", tc.min, tc.max)
			}
		})
	}
}

func TestSeedVNIRangeZeroDefaults(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if err := s.SeedVNIRange(ctx, 0, 0); err != nil {
		t.Fatalf("SeedVNIRange(0,0): %v", err)
	}
	min, max, err := s.VNIRange(ctx)
	if err != nil {
		t.Fatalf("VNIRange: %v", err)
	}
	if min != 1000 || max != 65535 {
		t.Errorf("VNIRange() after zero seed = (%d,%d), want (1000,65535)", min, max)
	}
}
