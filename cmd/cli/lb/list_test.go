// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestPrintLoadBalancerTable_StatusAndTargets locks the list table columns: the
// header carries STATUS and TARGETS, a load balancer with a health summary
// renders its status and healthy/total counts, and a load balancer without one
// (health nil, e.g. a fetch error for that owner) renders "-"/"-".
func TestPrintLoadBalancerTable_StatusAndTargets(t *testing.T) {
	t.Parallel()

	lbs := cpclient.LoadBalancerList{
		Data: []cpclient.LoadBalancer{
			{
				Name:     "web",
				Port:     8080,
				Selector: map[string]string{"app": "web"},
				Health: &cpclient.LoadBalancerHealthSummary{
					Status:         "degraded",
					TargetsTotal:   2,
					TargetsHealthy: 1,
				},
			},
			{
				Name:     "api",
				Port:     9090,
				Selector: map[string]string{"app": "api"},
				Health:   nil,
			},
		},
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printLoadBalancerTable(cmd, lbs, false)
	got := out.String()

	for _, want := range []string{"STATUS", "TARGETS", "degraded", "1/2"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}

	// The health-less row renders placeholder dashes for both columns.
	var apiLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "api") {
			apiLine = line
		}
	}
	if apiLine == "" {
		t.Fatalf("no api row rendered:\n%s", got)
	}
	if !strings.Contains(apiLine, "-") {
		t.Errorf("health-less row missing dash placeholders: %q", apiLine)
	}
}
