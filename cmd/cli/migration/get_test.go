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

func renderMigrationText(t *testing.T, m cpclient.Migration) string {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printMigrationText(cmd, m)
	return buf.String()
}

func lineFor(out, key string) (string, bool) {
	for ln := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(ln, key+": "); ok {
			return v, true
		}
	}
	return "", false
}

// TestPrintMigrationTextPendingWaiting covers the spec rule that a pending
// migration is NOT an error: when phase==pending and a scheduling_reason is
// set, the text view surfaces it as a `waiting:` line (not an `error:` line).
func TestPrintMigrationTextPendingWaiting(t *testing.T) {
	reason := "no_eligible_target"
	out := renderMigrationText(t, cpclient.Migration{
		ID:               "m1",
		Phase:            "pending",
		SchedulingReason: &reason,
	})
	if got, ok := lineFor(out, "waiting"); !ok || got != reason {
		t.Errorf("waiting line = %q (present=%v), want %q", got, ok, reason)
	}
	if _, ok := lineFor(out, "error"); ok {
		t.Errorf("error line present for a pending migration, want absent")
	}
	if got, ok := lineFor(out, "phase"); !ok || got != "pending" {
		t.Errorf("phase line = %q (present=%v), want pending", got, ok)
	}
}

// TestPrintMigrationTextActive covers a running migration: progress is shown
// with a percent sign, the VM and node NAMES render (not the ids), the id is
// shortened to its 8-char first segment, and no waiting line appears.
func TestPrintMigrationTextActive(t *testing.T) {
	srcID := "11111111-1111-1111-1111-111111111111"
	tgtID := "22222222-2222-2222-2222-222222222222"
	srcName := "node-a"
	tgtName := "node-b"
	out := renderMigrationText(t, cpclient.Migration{
		ID:              "abcdef0123456789-rest-of-uuid",
		VMID:            "vm-uuid",
		VMName:          "web-01",
		Phase:           "active",
		ProgressPercent: 47,
		SourceNodeID:    &srcID,
		SourceNodeName:  &srcName,
		TargetNodeID:    &tgtID,
		TargetNodeName:  &tgtName,
		Live:            true,
	})
	if got, ok := lineFor(out, "id"); !ok || got != "abcdef01" {
		t.Errorf("id line = %q (present=%v), want %q", got, ok, "abcdef01")
	}
	if got, ok := lineFor(out, "vm"); !ok || got != "web-01" {
		t.Errorf("vm line = %q (present=%v), want %q", got, ok, "web-01")
	}
	if got, ok := lineFor(out, "progress"); !ok || got != "47%" {
		t.Errorf("progress line = %q (present=%v), want 47%%", got, ok)
	}
	if got, ok := lineFor(out, "source_node"); !ok || got != srcName {
		t.Errorf("source_node = %q (present=%v), want %q", got, ok, srcName)
	}
	if got, ok := lineFor(out, "target_node"); !ok || got != tgtName {
		t.Errorf("target_node = %q (present=%v), want %q", got, ok, tgtName)
	}
	if _, ok := lineFor(out, "waiting"); ok {
		t.Errorf("waiting line present for an active migration, want absent")
	}
}

// TestPrintMigrationTextNodeLabelFallback covers the force-deleted-node case:
// a nil node name with a present id falls back to the id; both absent shows "-";
// and an empty vm_name falls back to the vm id.
func TestPrintMigrationTextNodeLabelFallback(t *testing.T) {
	srcID := "node-a-id"
	out := renderMigrationText(t, cpclient.Migration{
		ID:           "deadbeefcafef00d",
		VMID:         "vm-uuid-fallback",
		Phase:        "active",
		SourceNodeID: &srcID, // name nil, id present -> show id
		// target both nil -> "-"
	})
	if got, ok := lineFor(out, "source_node"); !ok || got != srcID {
		t.Errorf("source_node = %q (present=%v), want id fallback %q", got, ok, srcID)
	}
	if got, ok := lineFor(out, "target_node"); !ok || got != "-" {
		t.Errorf("target_node = %q (present=%v), want %q", got, ok, "-")
	}
	if got, ok := lineFor(out, "vm"); !ok || got != "vm-uuid-fallback" {
		t.Errorf("vm = %q (present=%v), want vm id fallback", got, ok)
	}
}

// TestPrintMigrationTextFailed covers a failed migration: the error_message is
// surfaced on an `error:` line.
func TestPrintMigrationTextFailed(t *testing.T) {
	msg := "copy aborted: agent unreachable"
	out := renderMigrationText(t, cpclient.Migration{
		ID:           "m3",
		Phase:        "failed",
		ErrorMessage: &msg,
	})
	if got, ok := lineFor(out, "error"); !ok || got != msg {
		t.Errorf("error line = %q (present=%v), want %q", got, ok, msg)
	}
}
