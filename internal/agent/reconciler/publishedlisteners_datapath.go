// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import "github.com/otherix/otherix/internal/agent/heartbeat"

// selectBackend picks a uniformly random backend from the CP-pushed set and
// returns it with true, or the zero value and false when the set is empty.
// rnd(n) must return a value in [0,n); production callers pass math/rand/v2's
// IntN, tests pass a deterministic stub.
//
// The set is selected in full: the CP already applied backend eligibility
// (fail toward inclusion) before pushing it, so DeclaredBackend.Healthy is
// informational and must not be re-filtered here - doing so would wrongly
// darken a warming-but-eligible backend.
func selectBackend(backends []heartbeat.DeclaredBackend, rnd func(int) int) (heartbeat.DeclaredBackend, bool) {
	if len(backends) == 0 {
		return heartbeat.DeclaredBackend{}, false
	}
	return backends[rnd(len(backends))], true
}
