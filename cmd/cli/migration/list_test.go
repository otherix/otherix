// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func renderMigrationTable(t *testing.T, migrations []cpclient.Migration, next string) string {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printMigrationTable(cmd, migrations, next)
	return buf.String()
}

// TestPrintMigrationTableNamesAndShortID covers the table view: the header
// carries the name columns, and each row shows the short migration id, the VM
// name and the source/target node names (with an id fallback for a node whose
// name is nil).
func TestPrintMigrationTableNamesAndShortID(t *testing.T) {
	srcName := "node-a"
	tgtID := "node-b-id"
	out := renderMigrationTable(t, []cpclient.Migration{
		{
			ID:              "abcdef0123456789-tail",
			VMName:          "web-01",
			SourceNodeName:  &srcName,
			TargetNodeID:    &tgtID, // name nil -> id fallback
			Phase:           "active",
			ProgressPercent: 30,
			CreatedAt:       "",
		},
	}, "")

	if !strings.Contains(out, "ID\tVM\tSOURCE\tTARGET\tSTATUS\tPROGRESS\tAGE") &&
		!headerHasColumns(out, "ID", "VM", "SOURCE", "TARGET", "STATUS", "PROGRESS", "AGE") {
		t.Errorf("table header missing name columns:\n%s", out)
	}
	// tabwriter pads with spaces, so assert on substrings of the data row.
	for _, want := range []string{"abcdef01", "web-01", "node-a", "node-b-id", "active", "30%"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
	// The full id must not leak into the table (json path carries it instead).
	if strings.Contains(out, "abcdef0123456789-tail") {
		t.Errorf("table leaked the full migration id:\n%s", out)
	}
}

// headerHasColumns checks the first output line contains every column label,
// tolerant of tabwriter space padding.
func headerHasColumns(out string, cols ...string) bool {
	header := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		header = out[:i]
	}
	for _, c := range cols {
		if !strings.Contains(header, c) {
			return false
		}
	}
	return true
}
