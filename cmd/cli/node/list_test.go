// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestRenderNodeStatus covers the STATUS-column computation of the
// node list view. Reachable nodes append "under_pressure" when any
// pressure condition is active; unreachable suppresses pressure
// rendering because heartbeat data — and pressure data derived from it
// — is stale.
func TestRenderNodeStatus(t *testing.T) {
	mem := &cpclient.PressureCondition{Since: "2026-05-12T12:00:00Z", ConsecutiveCount: 3}
	sys := &cpclient.PressureCondition{Since: "2026-05-12T12:05:00Z", ConsecutiveCount: 4}

	cases := []struct {
		name string
		node cpclient.Node
		want string
	}{
		{
			name: "ready, no pressure",
			node: cpclient.Node{Status: "ready"},
			want: "ready",
		},
		{
			name: "ready + memory pressure",
			node: cpclient.Node{Status: "ready", MemoryPressure: mem},
			want: "under_pressure",
		},
		{
			name: "ready + system_disk pressure",
			node: cpclient.Node{Status: "ready", SystemDiskPressure: sys},
			want: "under_pressure",
		},
		{
			name: "ready + both node-scoped pressures",
			node: cpclient.Node{Status: "ready", MemoryPressure: mem, SystemDiskPressure: sys},
			want: "under_pressure",
		},
		{
			name: "cordoned, no pressure",
			node: cpclient.Node{Status: "cordoned"},
			want: "cordoned",
		},
		{
			name: "cordoned + memory pressure",
			node: cpclient.Node{Status: "cordoned", MemoryPressure: mem},
			want: "cordoned, under_pressure",
		},
		{
			name: "cordoned + system_disk pressure",
			node: cpclient.Node{Status: "cordoned", SystemDiskPressure: sys},
			want: "cordoned, under_pressure",
		},
		{
			name: "pending, no pressure",
			node: cpclient.Node{Status: "pending"},
			want: "pending",
		},
		{
			name: "pending + system_disk pressure",
			node: cpclient.Node{Status: "pending", SystemDiskPressure: sys},
			want: "pending, under_pressure",
		},
		{
			name: "unreachable suppresses both pressure types",
			node: cpclient.Node{Status: "unreachable", MemoryPressure: mem, SystemDiskPressure: sys},
			want: "unreachable",
		},
		{
			name: "unreachable, no pressure",
			node: cpclient.Node{Status: "unreachable"},
			want: "unreachable",
		},
		{
			name: "gone untouched",
			node: cpclient.Node{Status: "gone"},
			want: "gone",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderNodeStatus(tc.node)
			if got != tc.want {
				t.Errorf("renderNodeStatus(%+v) = %q, want %q", tc.node, got, tc.want)
			}
		})
	}
}
