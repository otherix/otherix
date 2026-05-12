// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"time"

	"github.com/otherix/otherix/internal/config"
)

// poolPressureTransitionKind enumerates the outcomes of one
// computePoolDiskPressureTransition call. Mirrors the heartbeat-handler
// pressureTransitionKind (kept private к the package since the two
// packages do not share an enum). Callers use it к drive post-commit
// logging — only state-changing transitions emit lines.
type poolPressureTransitionKind int

const (
	poolPressureTransitionNone poolPressureTransitionKind = iota
	poolPressureTransitionCounting
	poolPressureTransitionSet
	poolPressureTransitionCleared
)

// computePoolDiskPressureTransition decides the next disk-pressure
// state для one storage pool from а single scan observation против
// the operator-configured detection knobs.
//
// Pure function, no side effects. Caller persists `(since, count)`
// through UpdatePoolDiskPressure inside the same transaction as
// UpsertStoragePoolUsage so scan metrics и pressure state always
// commit together.
//
// Semantics mirror the heartbeat-handler computePressureTransition
// (the asymmetric debouncing pattern is shared across all pressure
// types) but the scan path defaults к ConsecutiveRequired=1 since
// scans are minutes apart и а single observation is а sufficient
// signal. The function still honours larger ConsecutiveRequired values
// если the operator explicitly wants multi-scan debouncing.
//
//   - Disabled config OR missing metrics → carry current state
//     forward. А scan that reports NULL availability does not regress
//     existing pressure state.
//   - Below threshold → increment count; set pressure with `now` once
//     count reaches ConsecutiveRequired.
//   - At-or-above threshold → reset count к 0 и clear any active
//     pressure.
func computePoolDiskPressureTransition(
	currentSince *time.Time,
	currentCount int32,
	availableBytes *int64,
	capacityBytes *int64,
	cfg config.PressureConditionConfig,
	now time.Time,
) (*time.Time, int32, poolPressureTransitionKind) {
	if !cfg.Enabled || availableBytes == nil || capacityBytes == nil || *capacityBytes <= 0 {
		return currentSince, currentCount, poolPressureTransitionNone
	}

	percentAvailable := float64(*availableBytes) * 100.0 / float64(*capacityBytes)

	if percentAvailable < float64(cfg.ThresholdPercent) {
		newCount := currentCount + 1
		if currentSince == nil && int(newCount) >= cfg.ConsecutiveRequired {
			ts := now
			return &ts, newCount, poolPressureTransitionSet
		}
		if currentSince != nil {
			return currentSince, newCount, poolPressureTransitionNone
		}
		return nil, newCount, poolPressureTransitionCounting
	}

	if currentSince != nil {
		return nil, 0, poolPressureTransitionCleared
	}
	return nil, 0, poolPressureTransitionNone
}
