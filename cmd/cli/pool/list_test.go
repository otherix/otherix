// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestRenderPoolStatus covers the STATUS column worst-of priority
// composition. Priority order:
//
//	failed > unreachable > cordoned > under_pressure > pending > ready
//
// Cases pair every priority level in isolation and then walk
// cross-conditions to lock the order semantics. Orphaned pools (nil
// node_status) and other node lifecycle values (draining, gone)
// fall through to the pool-level checks per the rendering doc-
// comment.
func TestRenderPoolStatus(t *testing.T) {
	pressure := &cpclient.PressureCondition{Since: "2026-05-15T12:00:00Z", ConsecutiveCount: 1}

	cases := []struct {
		name string
		pool cpclient.Pool
		want string
	}{
		// --- six priority levels in isolation -------------------
		{
			name: "all clear",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("ready"),
				ReconciliationStatus: "ready",
			},
			want: "ready",
		},
		{
			name: "pending reconciliation",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("ready"),
				ReconciliationStatus: "pending",
			},
			want: "pending",
		},
		{
			name: "disk pressure only",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("ready"),
				ReconciliationStatus: "ready",
				DiskPressure:         pressure,
			},
			want: "under_pressure",
		},
		{
			name: "cordoned node",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("cordoned"),
				ReconciliationStatus: "ready",
			},
			want: "cordoned",
		},
		{
			name: "unreachable node",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("unreachable"),
				ReconciliationStatus: "ready",
			},
			want: "unreachable",
		},
		{
			name: "failed reconciliation",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("ready"),
				ReconciliationStatus: "failed",
			},
			want: "failed",
		},

		// --- cross-condition priority assertions ----------------
		{
			name: "failed trumps unreachable",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("unreachable"),
				ReconciliationStatus: "failed",
			},
			want: "failed",
		},
		{
			name: "failed trumps cordoned plus pressure",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("cordoned"),
				ReconciliationStatus: "failed",
				DiskPressure:         pressure,
			},
			want: "failed",
		},
		{
			name: "unreachable trumps cordoned",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("unreachable"),
				ReconciliationStatus: "ready",
			},
			want: "unreachable",
		},
		{
			name: "unreachable trumps pressure",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("unreachable"),
				ReconciliationStatus: "ready",
				DiskPressure:         pressure,
			},
			want: "unreachable",
		},
		{
			name: "cordoned trumps pressure",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("cordoned"),
				ReconciliationStatus: "ready",
				DiskPressure:         pressure,
			},
			want: "cordoned",
		},
		{
			name: "pressure trumps pending",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("ready"),
				ReconciliationStatus: "pending",
				DiskPressure:         pressure,
			},
			want: "under_pressure",
		},

		// --- fallthrough paths ----------------------------------
		{
			name: "orphaned pool with nil node status falls through",
			pool: cpclient.Pool{
				NodeStatus:           nil,
				ReconciliationStatus: "ready",
			},
			want: "ready",
		},
		{
			name: "orphaned pool with pending falls through to pending",
			pool: cpclient.Pool{
				NodeStatus:           nil,
				ReconciliationStatus: "pending",
			},
			want: "pending",
		},
		{
			name: "draining node falls through to pool-level checks",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("draining"),
				ReconciliationStatus: "ready",
			},
			want: "ready",
		},
		{
			name: "gone node with pressure falls through to under_pressure",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("gone"),
				ReconciliationStatus: "ready",
				DiskPressure:         pressure,
			},
			want: "under_pressure",
		},
		{
			name: "pending node with pending reconciliation surfaces pending",
			pool: cpclient.Pool{
				NodeStatus:           strPtr("pending"),
				ReconciliationStatus: "pending",
			},
			want: "pending",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderPoolStatus(tc.pool)
			if got != tc.want {
				t.Errorf("renderPoolStatus(%+v) = %q, want %q", tc.pool, got, tc.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
