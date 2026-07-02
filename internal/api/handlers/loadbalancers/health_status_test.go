// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import "testing"

// TestDeriveLBHealthStatus locks the aggregate-status rule: no backends,
// all-confirmed-healthy, all-confirmed-unhealthy, and every mix (including a
// serving-but-unconfirmed / warming set) collapse to the right label. A
// warming or partially-confirmed load balancer must read degraded, never
// unhealthy.
func TestDeriveLBHealthStatus(t *testing.T) {
	cases := []struct {
		name                      string
		total, healthy, unhealthy int
		want                      string
	}{
		{"no backends", 0, 0, 0, "no_backends"},
		{"all healthy", 3, 3, 0, "healthy"},
		{"all unhealthy", 2, 0, 2, "unhealthy"},
		{"mixed healthy and unhealthy", 2, 1, 1, "degraded"},
		{"all warming (no verdict)", 2, 0, 0, "degraded"},
		{"some healthy rest warming", 3, 1, 0, "degraded"},
		{"some unhealthy rest warming", 3, 0, 1, "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveLBHealthStatus(tc.total, tc.healthy, tc.unhealthy); got != tc.want {
				t.Errorf("deriveLBHealthStatus(%d, %d, %d) = %q, want %q",
					tc.total, tc.healthy, tc.unhealthy, got, tc.want)
			}
		})
	}
}
