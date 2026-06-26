// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// drainResultTask wraps a drainResult into the cpclient.Task shape
// renderDrainResult consumes, mirroring what WaitTask returns.
func drainResultTask(t *testing.T, status string, res drainResult) cpclient.Task {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal drain result: %v", err)
	}
	return cpclient.Task{Status: status, Result: raw}
}

func TestRenderDrainResult(t *testing.T) {
	const hint = "node left cordoned; add capacity and re-run drain"

	tests := []struct {
		name        string
		status      string
		result      drainResult
		wantSubstrs []string
		wantHint    bool
	}{
		{
			name:   "success no remaining",
			status: "success",
			result: drainResult{Migrated: 3, Remaining: 0, InFlight: 0},
			wantSubstrs: []string{
				"drain n1: success",
				"migrated=3 remaining=0 in_flight=0",
			},
			wantHint: false,
		},
		{
			name:   "timeout with stuck VMs",
			status: "failed",
			result: drainResult{
				Code: "drain_timeout", Migrated: 1, Remaining: 2, InFlight: 1,
				Stuck: []string{"web-0", "db-0"},
			},
			wantSubstrs: []string{
				"drain n1: failed",
				"migrated=1 remaining=2 in_flight=1",
				"reason: drain_timeout",
				"web-0",
				"db-0",
			},
			wantHint: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)

			task := drainResultTask(t, tt.status, tt.result)
			if err := renderDrainResult(cmd, "n1", task); err != nil {
				t.Fatalf("renderDrainResult(n1) error = %v, want nil", err)
			}

			got := out.String()
			for _, want := range tt.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("renderDrainResult output missing %q\ngot:\n%s", want, got)
				}
			}
			hasHint := strings.Contains(got, hint)
			if hasHint != tt.wantHint {
				t.Errorf("renderDrainResult hint present = %v, want %v (remaining=%d)\ngot:\n%s",
					hasHint, tt.wantHint, tt.result.Remaining, got)
			}
		})
	}
}
