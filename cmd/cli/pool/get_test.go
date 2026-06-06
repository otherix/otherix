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
				Name: "pool-dev", Type: "local_dir", IsClusterDefault: true,
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

// TestRenderInstanceImages asserts the flat per-instance view renders an
// `images:` section listing each cached image's name, short sha, and
// human-readable size when the pool carries images[].
func TestRenderInstanceImages(t *testing.T) {
	p := cpclient.Pool{
		ID: uuid.New(), Node: "node-1", Name: "pool-dev",
		Type: "local_dir", Path: "/opt/otherix/pools/default",
		Images: []cpclient.PoolImage{{
			Name:      "ubuntu-noble.qcow2",
			SHA256:    "abcdef0123456789aaaa",
			SizeBytes: 2 * 1024 * 1024 * 1024,
			Format:    "qcow2",
		}},
	}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderInstance(cmd, p); err != nil {
		t.Fatalf("renderInstance() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"images:\n", "ubuntu-noble.qcow2", "sha abcdef012345", "2.0GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderInstance output missing %q\n%s", want, out)
		}
	}
}

// TestPrintJSONPassthrough guards the fidelity contract: printJSON echoes the
// raw server body verbatim (re-indented) rather than re-marshaling a decoded
// struct. A clear pool's null disk_pressure (a scheduler eligibility gate)
// previously vanished because the cpclient struct carried omitempty; the
// passthrough keeps absent-vs-null exactly as the server decided.
func TestPrintJSONPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"name":"pool-dev","disk_pressure":null}`)
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
