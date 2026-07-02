// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestPrintNodeTableColumns locks the column contract of the default
// node table output: NAME ARCH STATUS CORDONED AGE (ARCHITECTURE
// abbreviated to ARCH, LAST_HEARTBEAT dropped). The ID column is
// prepended only under --show-ids.
func TestPrintNodeTableColumns(t *testing.T) {
	beat := "2026-05-12T12:00:00Z"
	list := cpclient.NodeList{Data: []cpclient.Node{{
		ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "node-1",
		Architecture: "arm64", Status: "ready", CreatedAt: "2026-05-12T09:00:00Z",
		LastHeartbeatAt: &beat,
	}}}

	t.Run("default columns", func(t *testing.T) {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printNodeTable(cmd, list, false)

		header := strings.Fields(strings.Split(strings.TrimSpace(buf.String()), "\n")[0])
		want := []string{"NAME", "ROLES", "ARCH", "STATUS", "CORDONED", "AGE"}
		if strings.Join(header, " ") != strings.Join(want, " ") {
			t.Errorf("header = %v, want %v", header, want)
		}
	})

	t.Run("show-ids prepends ID", func(t *testing.T) {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printNodeTable(cmd, list, true)

		header := strings.Fields(strings.Split(strings.TrimSpace(buf.String()), "\n")[0])
		want := []string{"ID", "NAME", "ROLES", "ARCH", "STATUS", "CORDONED", "AGE"}
		if strings.Join(header, " ") != strings.Join(want, " ") {
			t.Errorf("header = %v, want %v", header, want)
		}
	})
}

// TestPrintNodeTableRoles verifies the ROLES column renders each node's
// comma-joined roles for both the gateway and hypervisor cases.
func TestPrintNodeTableRoles(t *testing.T) {
	list := cpclient.NodeList{Data: []cpclient.Node{
		{
			ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Name: "hv-1",
			Architecture: "arm64", Status: "ready", CreatedAt: "2026-05-12T09:00:00Z",
			Roles: []string{"hypervisor"},
		},
		{
			ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Name: "gw-1",
			Architecture: "arm64", Status: "ready", CreatedAt: "2026-05-12T09:00:00Z",
			Roles: []string{"gateway"},
		},
	}}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printNodeTable(cmd, list, false)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), buf.String())
	}
	hv := strings.Fields(lines[1])
	gw := strings.Fields(lines[2])
	if len(hv) < 2 || hv[1] != "hypervisor" {
		t.Errorf("hypervisor row roles column = %v, want hypervisor at index 1", hv)
	}
	if len(gw) < 2 || gw[1] != "gateway" {
		t.Errorf("gateway row roles column = %v, want gateway at index 1", gw)
	}
}

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
