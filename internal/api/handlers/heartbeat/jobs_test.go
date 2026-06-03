// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"testing"
	"time"
)

// TestReconcileConfig_Defaults pins the fallback values applied by
// withDefaults. Changing these affects every caller that ships a zero config.
func TestReconcileConfig_Defaults(t *testing.T) {
	cfg := ReconcileConfig{}.withDefaults()
	if cfg.StaleThreshold != defaultStaleThreshold {
		t.Errorf("StaleThreshold default = %v, want %v", cfg.StaleThreshold, defaultStaleThreshold)
	}
	if cfg.GoneGrace != defaultGoneGrace {
		t.Errorf("GoneGrace default = %v, want %v", cfg.GoneGrace, defaultGoneGrace)
	}
	if cfg.Interval != defaultInterval {
		t.Errorf("Interval default = %v, want %v", cfg.Interval, defaultInterval)
	}

	// Non-zero overrides survive.
	override := ReconcileConfig{StaleThreshold: 5 * time.Second, GoneGrace: 9 * time.Second, Interval: 7 * time.Second}.withDefaults()
	if override.StaleThreshold != 5*time.Second || override.GoneGrace != 9*time.Second || override.Interval != 7*time.Second {
		t.Errorf("withDefaults clobbered explicit values: %+v", override)
	}
}
