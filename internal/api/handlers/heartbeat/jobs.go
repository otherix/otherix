// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import "time"

const (
	defaultStaleThreshold = 90 * time.Second
	defaultInterval       = 30 * time.Second
)

// ReconcileConfig pins the timing knobs the node-health reconcile keys off.
// Zero values fall back to package defaults via withDefaults; the field exists
// as a seam so tests can pin compressed durations and cmd/api can wire
// APIConfig overrides through the same shape.
type ReconcileConfig struct {
	StaleThreshold time.Duration
	Interval       time.Duration
}

func (c ReconcileConfig) withDefaults() ReconcileConfig {
	out := c
	if out.StaleThreshold <= 0 {
		out.StaleThreshold = defaultStaleThreshold
	}
	if out.Interval <= 0 {
		out.Interval = defaultInterval
	}
	return out
}
