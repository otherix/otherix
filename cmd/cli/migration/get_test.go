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
// with a percent sign, the route nodes render, and no waiting line appears.
func TestPrintMigrationTextActive(t *testing.T) {
	src := "node-a"
	tgt := "node-b"
	out := renderMigrationText(t, cpclient.Migration{
		ID:              "m2",
		Phase:           "active",
		ProgressPercent: 47,
		SourceNodeID:    &src,
		TargetNodeID:    &tgt,
		Live:            true,
	})
	if got, ok := lineFor(out, "progress"); !ok || got != "47%" {
		t.Errorf("progress line = %q (present=%v), want 47%%", got, ok)
	}
	if got, ok := lineFor(out, "target_node_id"); !ok || got != tgt {
		t.Errorf("target_node_id = %q (present=%v), want %q", got, ok, tgt)
	}
	if _, ok := lineFor(out, "waiting"); ok {
		t.Errorf("waiting line present for an active migration, want absent")
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
