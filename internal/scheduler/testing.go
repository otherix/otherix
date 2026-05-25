// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package scheduler

// NewNodePressureErrorForTest constructs an ErrNoEligibleNodes-wrapped
// error carrying the supplied NodePressureDetail. Used by handler /
// CLI tests that need to exercise the `ExtractNodePressureDetail` path
// without spinning up the full SchedulePlacement saga; production code
// mints these errors only through `placeWithPressure`. The `ForTest`
// suffix flags the test-only intent — this symbol exists for cross-
// package test wiring (handlers/vms, cmd/cli) and nowhere else.
func NewNodePressureErrorForTest(detail NodePressureDetail) error {
	return &nodePressureError{Detail: detail}
}
