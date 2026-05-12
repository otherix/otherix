// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package scheduler

// NewNodePressureErrorForTest constructs an ErrNoEligibleNodes-wrapped
// error carrying the supplied NodePressureDetail. Used by handler /
// CLI tests that need к exercise the `ExtractNodePressureDetail` path
// без spinning up the full SchedulePlacement saga; production code
// мins these errors only through `placeWithPressure`. The `ForTest`
// suffix flags the test-only intent — это symbol exists для cross-
// package test wiring (handlers/vms, cmd/cli) и nowhere else.
func NewNodePressureErrorForTest(detail NodePressureDetail) error {
	return &nodePressureError{Detail: detail}
}
