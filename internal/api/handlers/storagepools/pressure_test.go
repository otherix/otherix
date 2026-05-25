// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"testing"
	"time"

	"github.com/otherix/otherix/internal/config"
)

// TestComputePoolDiskPressureTransition covers the scan-driven pool
// disk pressure transition logic. Default
// `consecutive_required=1` means a single below-threshold scan sets
// pressure — distinct from the heartbeat-driven path which requires 3
// consecutive observations. The function still honours larger
// ConsecutiveRequired values if the operator explicitly wants
// multi-scan debouncing.
func TestComputePoolDiskPressureTransition(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-30 * time.Minute)

	cfg := config.PressureConditionConfig{
		Enabled:             true,
		ThresholdPercent:    15,
		ConsecutiveRequired: 1,
	}

	capacity := int64(1024 * 1024 * 1024 * 1024) // 1 TiB
	low := int64(50 * 1024 * 1024 * 1024)        // 50 GiB (~5%)
	high := int64(500 * 1024 * 1024 * 1024)      // 500 GiB (50%)

	cases := []struct {
		name        string
		current     *time.Time
		count       int32
		avail       *int64
		capacity    *int64
		cfg         config.PressureConditionConfig
		wantSince   *time.Time
		wantCount   int32
		wantKind    poolPressureTransitionKind
		description string
	}{
		{
			name:        "single below-threshold scan sets pressure (consecutive=1)",
			current:     nil,
			count:       0,
			avail:       &low,
			capacity:    &capacity,
			cfg:         cfg,
			wantSince:   &now,
			wantCount:   1,
			wantKind:    poolPressureTransitionSet,
			description: "scan-driven default debouncing fires on first observation",
		},
		{
			name:        "above threshold while pressured clears",
			current:     &earlier,
			count:       1,
			avail:       &high,
			capacity:    &capacity,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    poolPressureTransitionCleared,
			description: "single recovery scan clears pressure",
		},
		{
			name:     "consecutive=3 counts mid-flight",
			current:  nil,
			count:    1,
			avail:    &low,
			capacity: &capacity,
			cfg: config.PressureConditionConfig{
				Enabled: true, ThresholdPercent: 15, ConsecutiveRequired: 3,
			},
			wantSince:   nil,
			wantCount:   2,
			wantKind:    poolPressureTransitionCounting,
			description: "explicit consecutive_required > 1 honoured (matches memory-style debouncing)",
		},
		{
			name:        "disabled config carries state forward",
			current:     &earlier,
			count:       5,
			avail:       &low,
			capacity:    &capacity,
			cfg:         config.PressureConditionConfig{Enabled: false, ThresholdPercent: 15, ConsecutiveRequired: 1},
			wantSince:   &earlier,
			wantCount:   5,
			wantKind:    poolPressureTransitionNone,
			description: "disabled detection preserves existing state",
		},
		{
			name:        "missing capacity is treated as missing metric",
			current:     nil,
			count:       0,
			avail:       &low,
			capacity:    nil,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    poolPressureTransitionNone,
			description: "scan that reports availability but not capacity is a no-op",
		},
		{
			name:        "above threshold not pressured — steady state",
			current:     nil,
			count:       0,
			avail:       &high,
			capacity:    &capacity,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    poolPressureTransitionNone,
			description: "common steady-state path produces no transition",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSince, gotCount, gotKind := computePoolDiskPressureTransition(
				tc.current, tc.count, tc.avail, tc.capacity, tc.cfg, now,
			)
			if gotKind != tc.wantKind {
				t.Errorf("kind = %v, want %v — %s", gotKind, tc.wantKind, tc.description)
			}
			if gotCount != tc.wantCount {
				t.Errorf("count = %d, want %d — %s", gotCount, tc.wantCount, tc.description)
			}
			switch {
			case tc.wantSince == nil && gotSince != nil:
				t.Errorf("since = %v, want nil — %s", *gotSince, tc.description)
			case tc.wantSince != nil && gotSince == nil:
				t.Errorf("since = nil, want non-nil — %s", tc.description)
			case tc.wantSince != nil && gotSince != nil && !gotSince.Equal(*tc.wantSince):
				t.Errorf("since = %v, want %v — %s", *gotSince, *tc.wantSince, tc.description)
			}
		})
	}
}
