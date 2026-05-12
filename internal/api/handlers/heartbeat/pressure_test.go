// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"testing"
	"time"

	"github.com/otherix/otherix/internal/config"
)

func TestComputePressureTransition(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-5 * time.Minute)

	cfg := config.PressureConditionConfig{
		Enabled:             true,
		ThresholdPercent:    10,
		ConsecutiveRequired: 3,
	}

	total := int64(16384)
	low := int64(1500)  // ~9.16% — below 10%
	high := int64(3000) // ~18.3% — at-or-above 10%

	cases := []struct {
		name        string
		current     *time.Time
		count       int32
		avail       *int64
		total       *int64
		cfg         config.PressureConditionConfig
		wantSince   *time.Time
		wantCount   int32
		wantKind    pressureTransitionKind
		description string
	}{
		{
			name:        "disabled — no change regardless of metrics",
			current:     nil,
			count:       0,
			avail:       &low,
			total:       &total,
			cfg:         config.PressureConditionConfig{Enabled: false, ThresholdPercent: 10, ConsecutiveRequired: 3},
			wantSince:   nil,
			wantCount:   0,
			wantKind:    pressureTransitionNone,
			description: "Enabled=false skips computation entirely",
		},
		{
			name:        "missing metrics — carries state forward",
			current:     &earlier,
			count:       5,
			avail:       nil,
			total:       &total,
			cfg:         cfg,
			wantSince:   &earlier,
			wantCount:   5,
			wantKind:    pressureTransitionNone,
			description: "nil avail keeps existing flag set",
		},
		{
			name:        "below threshold first time — counting",
			current:     nil,
			count:       0,
			avail:       &low,
			total:       &total,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   1,
			wantKind:    pressureTransitionCounting,
			description: "first below-threshold tick increments count",
		},
		{
			name:        "below threshold reaches consecutive — set",
			current:     nil,
			count:       2,
			avail:       &low,
			total:       &total,
			cfg:         cfg,
			wantSince:   &now,
			wantCount:   3,
			wantKind:    pressureTransitionSet,
			description: "third below-threshold tick stamps pressure_since",
		},
		{
			name:        "already pressured — keep count rising но кind=None",
			current:     &earlier,
			count:       3,
			avail:       &low,
			total:       &total,
			cfg:         cfg,
			wantSince:   &earlier,
			wantCount:   4,
			wantKind:    pressureTransitionNone,
			description: "subsequent below-threshold ticks bump count, не re-set",
		},
		{
			name:        "above threshold while pressured — clear",
			current:     &earlier,
			count:       4,
			avail:       &high,
			total:       &total,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    pressureTransitionCleared,
			description: "single above-threshold tick clears pressure",
		},
		{
			name:        "above threshold while not pressured — steady state",
			current:     nil,
			count:       0,
			avail:       &high,
			total:       &total,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    pressureTransitionNone,
			description: "no-op steady state — common case",
		},
		{
			name:        "above threshold resets counting mid-flight",
			current:     nil,
			count:       2,
			avail:       &high,
			total:       &total,
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    pressureTransitionNone,
			description: "transient breach below + recovery resets debounce",
		},
		{
			name:        "zero total guarded",
			current:     nil,
			count:       0,
			avail:       &low,
			total:       zeroInt64(),
			cfg:         cfg,
			wantSince:   nil,
			wantCount:   0,
			wantKind:    pressureTransitionNone,
			description: "total=0 is treated as missing metric",
		},
		{
			name:        "consecutive=1 sets immediately",
			current:     nil,
			count:       0,
			avail:       &low,
			total:       &total,
			cfg:         config.PressureConditionConfig{Enabled: true, ThresholdPercent: 10, ConsecutiveRequired: 1},
			wantSince:   &now,
			wantCount:   1,
			wantKind:    pressureTransitionSet,
			description: "consecutive_required=1 collapses debounce к first observation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSince, gotCount, gotKind := computePressureTransition(
				tc.current, tc.count, tc.avail, tc.total, tc.cfg, now,
			)
			if gotKind != tc.wantKind {
				t.Errorf("computePressureTransition() kind = %v, want %v — %s",
					gotKind, tc.wantKind, tc.description)
			}
			if gotCount != tc.wantCount {
				t.Errorf("computePressureTransition() count = %d, want %d — %s",
					gotCount, tc.wantCount, tc.description)
			}
			switch {
			case tc.wantSince == nil && gotSince != nil:
				t.Errorf("computePressureTransition() since = %v, want nil — %s", *gotSince, tc.description)
			case tc.wantSince != nil && gotSince == nil:
				t.Errorf("computePressureTransition() since = nil, want non-nil — %s", tc.description)
			case tc.wantSince != nil && gotSince != nil && !gotSince.Equal(*tc.wantSince):
				t.Errorf("computePressureTransition() since = %v, want %v — %s",
					*gotSince, *tc.wantSince, tc.description)
			}
		})
	}
}

func zeroInt64() *int64 {
	v := int64(0)
	return &v
}

// TestComputePressureTransition_SystemDiskBytes confirms the generic
// transition function applies equally к byte-level metrics (system_disk)
// as it does к MiB (memory). The same threshold percentage и debounce
// counter semantics hold; only the input unit changes.
func TestComputePressureTransition_SystemDiskBytes(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cfg := config.PressureConditionConfig{
		Enabled:             true,
		ThresholdPercent:    10,
		ConsecutiveRequired: 3,
	}

	total := int64(100 * 1024 * 1024 * 1024) // 100 GiB
	low := int64(5 * 1024 * 1024 * 1024)     // 5 GiB (5% — below threshold)
	high := int64(50 * 1024 * 1024 * 1024)   // 50 GiB (50% — above threshold)
	earlier := now.Add(-5 * time.Minute)

	// Below threshold ×3 sets pressure.
	since, count, kind := computePressureTransition(nil, 2, &low, &total, cfg, now)
	if kind != pressureTransitionSet {
		t.Errorf("first transition: kind = %v, want pressureTransitionSet", kind)
	}
	if since == nil || !since.Equal(now) {
		t.Errorf("first transition: since = %v, want %v", since, now)
	}
	if count != 3 {
		t.Errorf("first transition: count = %d, want 3", count)
	}

	// Recovery: above threshold while pressured clears.
	since, count, kind = computePressureTransition(&earlier, 3, &high, &total, cfg, now)
	if kind != pressureTransitionCleared {
		t.Errorf("recovery: kind = %v, want pressureTransitionCleared", kind)
	}
	if since != nil {
		t.Errorf("recovery: since = %v, want nil", since)
	}
	if count != 0 {
		t.Errorf("recovery: count = %d, want 0", count)
	}
}
