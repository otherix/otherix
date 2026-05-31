// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import "time"

// Default retention windows - the tasks resource has retention semantics of
// its own. These are the working defaults; future per-task-type overrides would
// land alongside the type-keyed change.
const (
	defaultCompletedRetention = 7 * 24 * time.Hour
	defaultFailedRetention    = 30 * 24 * time.Hour
)

// RetentionConfig holds the per-state retention durations the cleanup function
// keys deletion off. Zero values fall back to the package defaults via
// withDefaults; the field exists as a seam so tests can pin compressed
// durations and cmd/api can wire APIConfig overrides through the same shape.
type RetentionConfig struct {
	Completed time.Duration
	Failed    time.Duration
}

// withDefaults returns a copy of c with any zero-valued field replaced by the
// package default. Non-zero fields pass through unchanged.
func (c RetentionConfig) withDefaults() RetentionConfig {
	out := c
	if out.Completed <= 0 {
		out.Completed = defaultCompletedRetention
	}
	if out.Failed <= 0 {
		out.Failed = defaultFailedRetention
	}
	return out
}
