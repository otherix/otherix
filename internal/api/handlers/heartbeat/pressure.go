// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"time"

	"github.com/otherix/otherix/internal/config"
)

// pressureTransitionKind enumerates the outcomes of one
// computePressureTransition call. Callers use it to drive post-commit
// logging — only state-changing transitions (set / clear) emit lines
// so steady-state heartbeats stay quiet.
type pressureTransitionKind int

const (
	// pressureTransitionNone — the row's pressure state did not change
	// and no debounce-counter update is needed.
	pressureTransitionNone pressureTransitionKind = iota
	// pressureTransitionCounting — a below-threshold observation
	// incremented the count without yet crossing
	// ConsecutiveRequired; pressure remains inactive.
	pressureTransitionCounting
	// pressureTransitionSet — the count reached ConsecutiveRequired and
	// pressure was just stamped active.
	pressureTransitionSet
	// pressureTransitionCleared — pressure was active and the latest
	// observation recovered to / above the threshold; flag cleared.
	pressureTransitionCleared
)

// computePressureTransition decides the next pressure state for a
// single resource dimension from one fresh observation against the
// operator-configured detection knobs. Resource-agnostic —
// memory (MiB) and system_disk / pool disk (bytes) share the same
// signature because the percentage comparison divides out the unit.
//
// Pure function — inputs map deterministically to outputs, no side
// effects, no dependency on the caller's clock except via the `now`
// argument. Caller persists the returned `(since, count)` via a
// per-resource Update*Pressure query inside the same transaction as
// the rest of the projection.
//
// Semantics:
//
//   - Disabled config OR missing metrics (nil pointers, or total ≤ 0)
//     → carry the current state unchanged. Pressure does not regress
//     just because the observation skipped a metric; the existing flag
//     stays put and continues to exclude the node / pool from placement.
//
//   - Below threshold → increment count. If the post-increment count
//     reaches ConsecutiveRequired and pressure is not yet active, stamp
//     pressure with `now` (pressureTransitionSet). Otherwise carry the
//     existing since-pointer (idempotent for already-pressured rows).
//
//   - At-or-above threshold → reset count to 0. If pressure was active,
//     clear the since-pointer (pressureTransitionCleared). Otherwise
//     no change (pressureTransitionNone — common steady-state path).
//
// Asymmetric debouncing — slow to set (sustained breach required), fast
// to clear (single observation recovers) — is the explicit design
// choice.
func computePressureTransition(
	currentSince *time.Time,
	currentCount int32,
	available *int64,
	total *int64,
	cfg config.PressureConditionConfig,
	now time.Time,
) (*time.Time, int32, pressureTransitionKind) {
	if !cfg.Enabled || available == nil || total == nil || *total <= 0 {
		return currentSince, currentCount, pressureTransitionNone
	}

	percentAvailable := float64(*available) * 100.0 / float64(*total)

	if percentAvailable < float64(cfg.ThresholdPercent) {
		newCount := currentCount + 1
		if currentSince == nil && int(newCount) >= cfg.ConsecutiveRequired {
			ts := now
			return &ts, newCount, pressureTransitionSet
		}
		if currentSince != nil {
			return currentSince, newCount, pressureTransitionNone
		}
		return nil, newCount, pressureTransitionCounting
	}

	if currentSince != nil {
		return nil, 0, pressureTransitionCleared
	}
	return nil, 0, pressureTransitionNone
}
