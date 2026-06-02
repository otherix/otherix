// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestRenderConceptDiskPressure(t *testing.T) {
	since := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	cases := []struct {
		name string
		dp   *cpclient.PressureCondition
		want string
	}{
		{name: "ok", dp: nil, want: "    disk_pressure: ok\n"},
		{name: "active", dp: &cpclient.PressureCondition{Since: since, ConsecutiveCount: 3}, want: "    disk_pressure: active since "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := cpclient.PoolConceptView{
				Name: "pool-mvp", Type: "local_dir", IsClusterDefault: true,
				Instances: []cpclient.Pool{{
					ID: uuid.New(), Node: "node-1", Path: "/opt/otherix/pools/default",
					ReconciliationStatus: "ready", DiskPressure: tc.dp,
				}},
			}
			cmd := &cobra.Command{}
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			if err := renderConcept(cmd, v); err != nil {
				t.Fatalf("renderConcept() error = %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("renderConcept output missing %q\n%s", tc.want, buf.String())
			}
		})
	}
}

// TestPrintJSONPassthrough guards the fidelity contract: printJSON echoes the
// raw server body verbatim (re-indented) rather than re-marshaling a decoded
// struct. A clear pool's null disk_pressure (a scheduler eligibility gate)
// previously vanished because the cpclient struct carried omitempty; the
// passthrough keeps absent-vs-null exactly as the server decided.
func TestPrintJSONPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"name":"pool-mvp","disk_pressure":null}`)
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := printJSON(cmd, raw); err != nil {
		t.Fatalf("printJSON() error = %v", err)
	}
	if !strings.Contains(buf.String(), `"disk_pressure": null`) {
		t.Errorf("printJSON output missing \"disk_pressure\": null\n%s", buf.String())
	}
}
