// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// ClusterSettings is one of the six resolver.Querier lookups; the full
// cluster.Store and resolver-embedding assertions land once storage-pools,
// nodes and vms are implemented.

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

func TestClusterSettingsSetAndClearDefaultNetwork(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()

	cs, err := st.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings() error = %v", err)
	}
	if cs.DefaultNetworkName != nil {
		t.Errorf("DefaultNetworkName = %v, want nil", *cs.DefaultNetworkName)
	}

	name := "default"
	if err := st.SetDefaultNetworkName(ctx, &name); err != nil {
		t.Fatalf("SetDefaultNetworkName(%q) error = %v", name, err)
	}
	cs, err = st.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings() error = %v", err)
	}
	if cs.DefaultNetworkName == nil || *cs.DefaultNetworkName != name {
		t.Errorf("DefaultNetworkName = %v, want %q", cs.DefaultNetworkName, name)
	}

	// A sibling-field write must not clobber the network name (CAS merge).
	pool := "pool-a"
	if err := st.SetDefaultPoolName(ctx, &pool); err != nil {
		t.Fatalf("SetDefaultPoolName(%q) error = %v", pool, err)
	}
	cs, err = st.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings() error = %v", err)
	}
	if cs.DefaultNetworkName == nil || *cs.DefaultNetworkName != name {
		t.Errorf("after sibling write DefaultNetworkName = %v, want %q", cs.DefaultNetworkName, name)
	}

	if err := st.ClearDefaultNetworkName(ctx); err != nil {
		t.Fatalf("ClearDefaultNetworkName() error = %v", err)
	}
	cs, err = st.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings() error = %v", err)
	}
	if cs.DefaultNetworkName != nil {
		t.Errorf("DefaultNetworkName = %v, want nil after clear", *cs.DefaultNetworkName)
	}
}

func TestSeedDefaultPoolName(t *testing.T) {
	ctx := context.Background()

	t.Run("seed on fresh store sets the name", func(t *testing.T) {
		s, _ := startStore(t)
		if err := s.SeedDefaultPoolName(ctx, "default"); err != nil {
			t.Fatalf("SeedDefaultPoolName: %v", err)
		}
		got, err := s.ClusterSettings(ctx)
		if err != nil {
			t.Fatalf("ClusterSettings: %v", err)
		}
		if got.DefaultPoolName == nil || *got.DefaultPoolName != "default" {
			t.Errorf("DefaultPoolName = %v, want %q", got.DefaultPoolName, "default")
		}
	})

	t.Run("second seed with a different name is a no-op", func(t *testing.T) {
		// Revert-to-confirm: with writeClusterSettings (overwrite) the second
		// "other" value would win and this assertion would fail.
		s, _ := startStore(t)
		if err := s.SeedDefaultPoolName(ctx, "first"); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		if err := s.SeedDefaultPoolName(ctx, "other"); err != nil {
			t.Fatalf("second seed: %v", err)
		}
		got, err := s.ClusterSettings(ctx)
		if err != nil {
			t.Fatalf("ClusterSettings: %v", err)
		}
		if got.DefaultPoolName == nil || *got.DefaultPoolName != "first" {
			t.Errorf("DefaultPoolName = %v, want immutable %q", got.DefaultPoolName, "first")
		}
	})

	t.Run("empty name is a no-op", func(t *testing.T) {
		s, _ := startStore(t)
		if err := s.SeedDefaultPoolName(ctx, ""); err != nil {
			t.Fatalf("SeedDefaultPoolName(\"\"): %v", err)
		}
		got, err := s.ClusterSettings(ctx)
		if err != nil {
			t.Fatalf("ClusterSettings: %v", err)
		}
		if got.DefaultPoolName != nil {
			t.Errorf("DefaultPoolName = %v, want nil (operator opted out)", *got.DefaultPoolName)
		}
	})

	t.Run("seed does not clobber an operator-set value", func(t *testing.T) {
		s, _ := startStore(t)
		manual := "manual"
		if err := s.SetDefaultPoolName(ctx, &manual); err != nil {
			t.Fatalf("SetDefaultPoolName: %v", err)
		}
		if err := s.SeedDefaultPoolName(ctx, "default"); err != nil {
			t.Fatalf("SeedDefaultPoolName: %v", err)
		}
		got, err := s.ClusterSettings(ctx)
		if err != nil {
			t.Fatalf("ClusterSettings: %v", err)
		}
		if got.DefaultPoolName == nil || *got.DefaultPoolName != "manual" {
			t.Errorf("DefaultPoolName = %v, want operator-set %q", got.DefaultPoolName, "manual")
		}
	})
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

func TestSeedAndReadUnderlayMTU(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if err := s.SeedUnderlayMTU(ctx, 9000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mtu, err := s.UnderlayMTU(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mtu != 9000 {
		t.Errorf("underlay mtu = %d, want 9000", mtu)
	}
	if err := s.SeedUnderlayMTU(ctx, 1400); err != nil { // immutable: no-op
		t.Fatalf("second seed: %v", err)
	}
	mtu, _ = s.UnderlayMTU(ctx)
	if mtu != 9000 {
		t.Errorf("underlay mtu = %d after second seed, want immutable 9000", mtu)
	}
}

func TestUnderlayMTUDefaultWhenUnset(t *testing.T) {
	s, _ := startStore(t)
	mtu, err := s.UnderlayMTU(context.Background())
	if err != nil {
		t.Fatalf("UnderlayMTU: %v", err)
	}
	if mtu != 1500 {
		t.Errorf("UnderlayMTU() default = %d, want 1500", mtu)
	}
}

func TestSeedUnderlayMTUFloor(t *testing.T) {
	cases := []struct {
		name    string
		mtu     int
		wantErr bool
		want    int32 // expected UnderlayMTU when accepted
	}{
		{name: "just below floor", mtu: 1389, wantErr: true},
		{name: "at floor", mtu: 1390, want: 1390},
		{name: "classic ethernet", mtu: 1500, want: 1500},
		{name: "at ceiling", mtu: 65535, want: 65535},
		{name: "above ceiling", mtu: 65536, wantErr: true},
		{name: "zero defaults", mtu: 0, want: 1500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := startStore(t)
			ctx := context.Background()
			err := s.SeedUnderlayMTU(ctx, tc.mtu)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SeedUnderlayMTU(%d) = nil, want error", tc.mtu)
				}
				return
			}
			if err != nil {
				t.Fatalf("SeedUnderlayMTU(%d) = %v, want nil", tc.mtu, err)
			}
			got, err := s.UnderlayMTU(ctx)
			if err != nil {
				t.Fatalf("UnderlayMTU: %v", err)
			}
			if got != tc.want {
				t.Errorf("UnderlayMTU() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSeedUnderlayMTUFloorErrorMentionsFloor(t *testing.T) {
	s, _ := startStore(t)
	err := s.SeedUnderlayMTU(context.Background(), 1389)
	if err == nil {
		t.Fatalf("SeedUnderlayMTU(1389) = nil, want error")
	}
	if !strings.Contains(err.Error(), "1390") {
		t.Errorf("SeedUnderlayMTU(1389) error = %q, want it to mention the 1390 floor", err)
	}
}

// TestSeedClusterSettingsConcurrentFieldsDoNotClobber boots two seeders against
// the same fresh singleton, each setting a DIFFERENT field. Under a blind
// read-modify-Put both read the same all-nil object and the last Put wins on the
// whole document, erasing the other writer's field. The CAS path serialises the
// two writers so both fields survive. Run under -race.
func TestSeedClusterSettingsConcurrentFieldsDoNotClobber(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	var mtuErr, vniErr error
	go func() {
		defer wg.Done()
		mtuErr = s.SeedUnderlayMTU(ctx, 9000)
	}()
	go func() {
		defer wg.Done()
		vniErr = s.SeedVNIRange(ctx, 2000, 3000)
	}()
	wg.Wait()

	if mtuErr != nil {
		t.Fatalf("SeedUnderlayMTU: %v", mtuErr)
	}
	if vniErr != nil {
		t.Fatalf("SeedVNIRange: %v", vniErr)
	}

	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		t.Fatalf("ClusterSettings: %v", err)
	}
	if cs.UnderlayMTU == nil || *cs.UnderlayMTU != 9000 {
		t.Errorf("UnderlayMTU = %v, want 9000 (clobbered by concurrent VNI seed)", cs.UnderlayMTU)
	}
	if cs.VNIMin == nil || *cs.VNIMin != 2000 {
		t.Errorf("VNIMin = %v, want 2000 (clobbered by concurrent MTU seed)", cs.VNIMin)
	}
}

func TestDrainSettingsDefaults(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	to, err := s.DrainTimeoutSeconds(ctx)
	if err != nil {
		t.Fatalf("DrainTimeoutSeconds: %v", err)
	}
	if to != 600 {
		t.Errorf("default drain timeout = %d, want 600", to)
	}

	conc, err := s.DrainMaxConcurrentMigrations(ctx)
	if err != nil {
		t.Fatalf("DrainMaxConcurrentMigrations: %v", err)
	}
	if conc != 2 {
		t.Errorf("default drain concurrency = %d, want 2", conc)
	}
}

func TestSeedUnderlayMTUZeroDefaults(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if err := s.SeedUnderlayMTU(ctx, 0); err != nil {
		t.Fatalf("SeedUnderlayMTU(0): %v", err)
	}
	mtu, err := s.UnderlayMTU(ctx)
	if err != nil {
		t.Fatalf("UnderlayMTU: %v", err)
	}
	if mtu != 1500 {
		t.Errorf("UnderlayMTU() after zero seed = %d, want 1500", mtu)
	}
}

func TestSSHIngressSettingsDefaults(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	// An unconfigured cluster defaults the master switch ON so the per-VM
	// opt-in works without an extra cluster step.
	on, err := s.SSHIngressEnabled(ctx)
	if err != nil {
		t.Fatalf("SSHIngressEnabled: %v", err)
	}
	if !on {
		t.Errorf("default SSHIngressEnabled = %v, want true", on)
	}

	suffix, err := s.SSHClusterSuffix(ctx)
	if err != nil {
		t.Fatalf("SSHClusterSuffix: %v", err)
	}
	if suffix != store.DefaultSSHClusterSuffix {
		t.Errorf("default SSHClusterSuffix = %q, want %q", suffix, store.DefaultSSHClusterSuffix)
	}
}

// TestSSHIngressSettingsExplicitDisable confirms an operator who explicitly
// stores enabled=false turns the default-ON switch off, distinguishing
// "never configured" (nil -> ON) from "explicitly disabled" (false -> OFF).
func TestSSHIngressSettingsExplicitDisable(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if err := s.SetSSHIngress(ctx, false, nil); err != nil {
		t.Fatalf("SetSSHIngress(false): %v", err)
	}
	on, err := s.SSHIngressEnabled(ctx)
	if err != nil {
		t.Fatalf("SSHIngressEnabled: %v", err)
	}
	if on {
		t.Errorf("explicitly disabled SSHIngressEnabled = %v, want false", on)
	}
}

func TestSSHIngressSettingsSetAndRead(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	suffix := "vms.example.test"
	if err := s.SetSSHIngress(ctx, true, &suffix); err != nil {
		t.Fatalf("SetSSHIngress(true): %v", err)
	}

	on, err := s.SSHIngressEnabled(ctx)
	if err != nil {
		t.Fatalf("SSHIngressEnabled: %v", err)
	}
	if !on {
		t.Errorf("SSHIngressEnabled after set = %v, want true", on)
	}
	got, err := s.SSHClusterSuffix(ctx)
	if err != nil {
		t.Fatalf("SSHClusterSuffix: %v", err)
	}
	if got != suffix {
		t.Errorf("SSHClusterSuffix after set = %q, want %q", got, suffix)
	}

	// A sibling-field write must not clobber the SSH settings (CAS merge).
	pool := "pool-a"
	if err := s.SetDefaultPoolName(ctx, &pool); err != nil {
		t.Fatalf("SetDefaultPoolName(%q): %v", pool, err)
	}
	on, err = s.SSHIngressEnabled(ctx)
	if err != nil {
		t.Fatalf("SSHIngressEnabled after sibling write: %v", err)
	}
	if !on {
		t.Errorf("after sibling write SSHIngressEnabled = %v, want true", on)
	}
	got, err = s.SSHClusterSuffix(ctx)
	if err != nil {
		t.Fatalf("SSHClusterSuffix after sibling write: %v", err)
	}
	if got != suffix {
		t.Errorf("after sibling write SSHClusterSuffix = %q, want %q", got, suffix)
	}

	// Disabling clears the suffix and flips the switch in one atomic write. A
	// cleared (nil) suffix resolves back to the default through the accessor.
	if err := s.SetSSHIngress(ctx, false, nil); err != nil {
		t.Fatalf("SetSSHIngress(false): %v", err)
	}
	got, err = s.SSHClusterSuffix(ctx)
	if err != nil {
		t.Fatalf("SSHClusterSuffix after clear: %v", err)
	}
	if got != store.DefaultSSHClusterSuffix {
		t.Errorf("SSHClusterSuffix after clear = %q, want %q", got, store.DefaultSSHClusterSuffix)
	}
	on, err = s.SSHIngressEnabled(ctx)
	if err != nil {
		t.Fatalf("SSHIngressEnabled after disable: %v", err)
	}
	if on {
		t.Errorf("SSHIngressEnabled after disable = %v, want false", on)
	}
}
