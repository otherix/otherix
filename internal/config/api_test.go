// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"strings"
	"testing"
	"time"
)

func TestResourcesConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ResourcesConfig
		wantErr string
	}{
		{name: "production defaults", cfg: defaultResourcesConfig()},
		{
			name: "all disabled, ratios still positive",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
				Memory: ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
				Disk:   ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
			},
		},
		{
			name: "mixed overcommit ratios",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 2.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: 0.8},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			},
		},
		{
			name: "cpu zero ratio (disabled)",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: false, OvercommitRatio: 0.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			},
			wantErr: "placement.resources.cpu.overcommit_ratio must be > 0",
		},
		{
			name: "memory negative ratio",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: -1.0},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			},
			wantErr: "placement.resources.memory.overcommit_ratio must be > 0",
		},
		{
			name: "disk zero ratio (enabled)",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 0.0},
			},
			wantErr: "placement.resources.disk.overcommit_ratio must be > 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPressureConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PressureConfig
		wantErr string
	}{
		{name: "defaults pass", cfg: defaultPressureConfig()},
		{
			name: "threshold below range",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: true, ThresholdPercent: 0, ConsecutiveRequired: 3,
			}},
			wantErr: "placement.pressure.memory.threshold_percent must be in [1, 99]",
		},
		{
			name: "threshold above range",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: true, ThresholdPercent: 100, ConsecutiveRequired: 3,
			}},
			wantErr: "placement.pressure.memory.threshold_percent must be in [1, 99]",
		},
		{
			name: "consecutive zero",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: true, ThresholdPercent: 10, ConsecutiveRequired: 0,
			}},
			wantErr: "placement.pressure.memory.consecutive_required must be >= 1",
		},
		{
			name: "enabled false still validates ranges (config hygiene)",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: false, ThresholdPercent: 200, ConsecutiveRequired: 3,
			}},
			wantErr: "placement.pressure.memory.threshold_percent must be in [1, 99]",
		},
		{
			name: "system_disk threshold below range",
			cfg: PressureConfig{
				Memory:     defaultPressureConfig().Memory,
				SystemDisk: PressureConditionConfig{Enabled: true, ThresholdPercent: 0, ConsecutiveRequired: 3},
				Disk:       defaultPressureConfig().Disk,
			},
			wantErr: "placement.pressure.system_disk.threshold_percent must be in [1, 99]",
		},
		{
			name: "disk consecutive zero",
			cfg: PressureConfig{
				Memory:     defaultPressureConfig().Memory,
				SystemDisk: defaultPressureConfig().SystemDisk,
				Disk:       PressureConditionConfig{Enabled: true, ThresholdPercent: 15, ConsecutiveRequired: 0},
			},
			wantErr: "placement.pressure.disk.consecutive_required must be >= 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestPlacementConfig_Validate_ResourcesPropagated(t *testing.T) {
	// PlacementConfig.Validate must surface ResourcesConfig errors so
	// the api-server fails fast at startup rather than on the hot path.
	cfg := PlacementConfig{
		Algorithm: "resource_aware",
		Resources: ResourcesConfig{
			CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			Memory: ResourceConfig{Enabled: true, OvercommitRatio: -1.0},
			Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "memory.overcommit_ratio") {
		t.Errorf("Validate() = %v, want memory.overcommit_ratio error", err)
	}
}

func TestPlacementConfig_Warnings(t *testing.T) {
	// Each case asserts the operator-facing warning content surfaced
	// by `Warnings()`. Substrings are checked rather than full strings
	// so the format helper can evolve without touching every case.
	tests := []struct {
		name     string
		cfg      PlacementConfig
		wantLen  int
		contains []string
	}{
		{
			name:    "defaults — no warnings",
			cfg:     PlacementConfig{Algorithm: "resource_aware", Resources: defaultResourcesConfig()},
			wantLen: 0,
		},
		{
			name: "cpu overcommit 1.5 — neutral language",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.5},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"cpu.overcommit_ratio=1.50", "overcommit enabled"},
		},
		{
			name: "memory overcommit 1.2 — OOM risk language",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.2},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"memory.overcommit_ratio=1.20", "OOM kill risk"},
		},
		{
			name: "memory overcommit 2.5 — extreme bucket",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 2.5},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"memory.overcommit_ratio=2.50", "extreme overcommit", "OOM kill risk"},
		},
		{
			name: "disk overcommit 1.3 — sparse qcow2 risk",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.3},
				},
			},
			wantLen:  1,
			contains: []string{"disk.overcommit_ratio=1.30", "sparse qcow2"},
		},
		{
			name: "memory overcommit ignored when disabled",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: false, OvercommitRatio: 3.0},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen: 0,
		},
		{
			name: "all three disabled — count fallback warning",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
					Disk:   ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"all dimensions disabled", "count-based"},
		},
		{
			name: "mixed: cpu overcommit + memory extreme + all-disabled not triggered",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.5},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 2.5},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  2,
			contains: []string{"cpu.overcommit_ratio=1.50", "memory.overcommit_ratio=2.50", "extreme overcommit"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Warnings()
			if len(got) != tc.wantLen {
				t.Fatalf("Warnings() returned %d entries, want %d\nentries=%q", len(got), tc.wantLen, got)
			}
			joined := strings.Join(got, "\n")
			for _, sub := range tc.contains {
				if !strings.Contains(joined, sub) {
					t.Errorf("Warnings() missing substring %q in %q", sub, joined)
				}
			}
		})
	}
}

func TestStoragePoolScanConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     StoragePoolScanConfig
		wantErr string
	}{
		{
			name: "production defaults",
			cfg:  StoragePoolScanConfig{Enabled: true, Interval: 15 * time.Minute, Jitter: 30 * time.Second},
		},
		{
			name: "zero values fall through to worker defaults",
			cfg:  StoragePoolScanConfig{},
		},
		{
			name: "disabled with non-zero timings",
			cfg:  StoragePoolScanConfig{Enabled: false, Interval: time.Hour, Jitter: time.Second},
		},
		{
			name:    "negative interval",
			cfg:     StoragePoolScanConfig{Interval: -time.Second},
			wantErr: "interval must be >= 0",
		},
		{
			name:    "negative jitter",
			cfg:     StoragePoolScanConfig{Interval: time.Minute, Jitter: -time.Second},
			wantErr: "jitter must be >= 0",
		},
		{
			name:    "jitter equal interval",
			cfg:     StoragePoolScanConfig{Interval: time.Minute, Jitter: time.Minute},
			wantErr: "must be less than interval",
		},
		{
			name:    "jitter exceeds interval",
			cfg:     StoragePoolScanConfig{Interval: time.Minute, Jitter: 2 * time.Minute},
			wantErr: "must be less than interval",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
