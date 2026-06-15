// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"testing"
	"time"
)

// TestNextDelay pins the backoff curve deterministically (no wall-clock): each
// step doubles until it would exceed max, then clamps to max and stays there.
// This is the authoritative growth check; the PollTask integration test only
// asserts jitter-safe lower bounds on the observed sleeps.
func TestNextDelay(t *testing.T) {
	t.Parallel()
	ms := time.Millisecond
	cases := []struct {
		name         string
		current, max time.Duration
		want         time.Duration
	}{
		{"doubles below cap", 10 * ms, 400 * ms, 20 * ms},
		{"doubles below cap again", 20 * ms, 400 * ms, 40 * ms},
		{"doubles to exactly cap", 200 * ms, 400 * ms, 400 * ms},
		{"clamps when double exceeds cap", 300 * ms, 400 * ms, 400 * ms},
		{"stays at cap", 400 * ms, 400 * ms, 400 * ms},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nextDelay(c.current, c.max); got != c.want {
				t.Errorf("nextDelay(%v, %v) = %v, want %v", c.current, c.max, got, c.want)
			}
		})
	}
}
