// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool

import (
	"bytes"
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
			if err := renderConcept(cmd, v, "text"); err != nil {
				t.Fatalf("renderConcept() error = %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("renderConcept output missing %q\n%s", tc.want, buf.String())
			}
		})
	}
}

// TestRenderConceptJSONIncludesDiskPressure guards against the cpclient
// omitempty that previously dropped a nil disk_pressure from --output json,
// hiding a scheduler eligibility gate. A clear pool must render the field as
// null, consistent with reconciliation_error and the server response.
func TestRenderConceptJSONIncludesDiskPressure(t *testing.T) {
	v := cpclient.PoolConceptView{
		Name: "pool-mvp", Type: "local_dir",
		Instances: []cpclient.Pool{{ID: uuid.New(), Node: "node-1", DiskPressure: nil}},
	}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderConcept(cmd, v, "json"); err != nil {
		t.Fatalf("renderConcept(json) error = %v", err)
	}
	if !strings.Contains(buf.String(), `"disk_pressure": null`) {
		t.Errorf("--output json missing \"disk_pressure\": null\n%s", buf.String())
	}
}
