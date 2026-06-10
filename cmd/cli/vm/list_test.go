// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestRenderVMNetwork covers the NETWORK-column computation of the vm
// list view: no NIC renders "-", a single network renders its name,
// and multiple networks render the primary name plus a "(+N)" counter
// of the remaining NICs.
func TestRenderVMNetwork(t *testing.T) {
	cases := []struct {
		name     string
		networks []string
		want     string
	}{
		{name: "no nic", networks: nil, want: "-"},
		{name: "empty slice", networks: []string{}, want: "-"},
		{name: "single", networks: []string{"net-a"}, want: "net-a"},
		{name: "two", networks: []string{"net-a", "net-b"}, want: "net-a (+1)"},
		{name: "three", networks: []string{"net-a", "net-b", "net-c"}, want: "net-a (+2)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderVMNetwork(tc.networks)
			if got != tc.want {
				t.Errorf("renderVMNetwork(%v) = %q, want %q", tc.networks, got, tc.want)
			}
		})
	}
}

// TestPrintVMTableColumns locks the column contract of the default
// table output: NAME STATUS ARCH NODE POOL NETWORK AGE (no IMAGE), with
// the ID column prepended only under --show-ids.
func TestPrintVMTableColumns(t *testing.T) {
	node := "node-1"
	list := cpclient.VMList{Data: []cpclient.VM{{
		ID: "11111111-1111-1111-1111-111111111111", Name: "vm-a", Status: cpclient.VMStatus{Phase: "running"},
		Architecture: "arm64", Node: &node, Pool: "default", Networks: []string{"net-a", "net-b"},
	}}}

	t.Run("default columns", func(t *testing.T) {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printVMTable(cmd, list, false)

		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		header := strings.Fields(lines[0])
		want := []string{"NAME", "STATUS", "ARCH", "NODE", "POOL", "NETWORK", "AGE"}
		if strings.Join(header, " ") != strings.Join(want, " ") {
			t.Errorf("header = %v, want %v", header, want)
		}
		row := strings.Fields(lines[1])
		// "net-a (+1)" splits into two fields; the VM carries no created_at,
		// so the AGE cell renders the "-" placeholder.
		wantRow := []string{"vm-a", "running", "arm64", "node-1", "default", "net-a", "(+1)", "-"}
		if strings.Join(row, " ") != strings.Join(wantRow, " ") {
			t.Errorf("row = %v, want %v", row, wantRow)
		}
	})

	t.Run("show-ids prepends ID", func(t *testing.T) {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printVMTable(cmd, list, true)

		header := strings.Fields(strings.Split(strings.TrimSpace(buf.String()), "\n")[0])
		want := []string{"ID", "NAME", "STATUS", "ARCH", "NODE", "POOL", "NETWORK", "AGE"}
		if strings.Join(header, " ") != strings.Join(want, " ") {
			t.Errorf("header = %v, want %v", header, want)
		}
	})
}

// TestPrintVMTableAge covers the AGE cell: a VM with a created_at renders a
// compact age token (the trailing cell), while a VM with no created_at
// renders the "-" placeholder.
func TestPrintVMTableAge(t *testing.T) {
	created := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	list := cpclient.VMList{Data: []cpclient.VM{{
		Name: "vm-aged", Status: cpclient.VMStatus{Phase: "running"}, Architecture: "amd64",
		Pool: "default", CreatedAt: created,
	}}}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printVMTable(cmd, list, false)

	row := strings.Fields(strings.Split(strings.TrimSpace(buf.String()), "\n")[1])
	age := row[len(row)-1]
	if age != "3h" {
		t.Errorf("AGE cell = %q, want %q", age, "3h")
	}
}

// TestPrintVMTableNilNode renders the placeholder for a VM that has no
// observed node yet (still creating) and no NIC.
func TestPrintVMTableNilNode(t *testing.T) {
	list := cpclient.VMList{Data: []cpclient.VM{{
		Name: "vm-b", Status: cpclient.VMStatus{Phase: "creating"}, Architecture: "amd64", Node: nil, Pool: "default",
	}}}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printVMTable(cmd, list, false)

	row := strings.Fields(strings.Split(strings.TrimSpace(buf.String()), "\n")[1])
	// No node, no NIC, no created_at -> three "-" placeholders (NODE, NETWORK, AGE).
	want := []string{"vm-b", "creating", "amd64", "-", "default", "-", "-"}
	if strings.Join(row, " ") != strings.Join(want, " ") {
		t.Errorf("row = %v, want %v", row, want)
	}
}
