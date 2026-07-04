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

// TestRenderGet_HealthCheckAndBackends locks the text render of the
// health_check block and the per-backend backends list: effective
// health-check values are printed, a healthy backend shows its verdict and
// probe timestamp, and a warming backend (nil healthy / nil reported_at)
// renders healthy=unknown last_probed=-.
func TestRenderGet_HealthCheckAndBackends(t *testing.T) {
	t.Parallel()
	port, interval, timeout, ht, ut := 8080, 5, 2, 2, 3
	healthy := true
	reported := "2026-07-01T10:00:00Z"
	lb := cpclient.LoadBalancer{
		ID:       "id-1",
		Name:     "web",
		OwnerID:  "owner-1",
		Port:     80,
		Selector: map[string]string{"app": "web"},
		HealthCheck: cpclient.HealthCheck{
			Port:               &port,
			IntervalSeconds:    &interval,
			TimeoutSeconds:     &timeout,
			HealthyThreshold:   &ht,
			UnhealthyThreshold: &ut,
		},
		Backends: []cpclient.Backend{
			{VMID: "v1", VMName: "web-1", Healthy: &healthy, ReportedAt: &reported},
			{VMID: "v2", VMName: "web-2", Healthy: nil, ReportedAt: nil},
		},
		CreatedAt: "2026-07-01T10:00:00Z",
		UpdatedAt: "2026-07-01T10:00:00Z",
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderGet(cmd, lb); err != nil {
		t.Fatalf("renderGet: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"health_check:",
		"port: 8080",
		"interval_seconds: 5",
		"timeout_seconds: 2",
		"healthy_threshold: 2",
		"unhealthy_threshold: 3",
		"backends:",
		"- web-1  healthy=true  last_probed=2026-07-01T10:00:00Z",
		"- web-2  healthy=unknown  last_probed=-",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

// TestRenderGet_Listeners asserts the text render lists the observed per-gateway
// listeners of a published load balancer, each line showing the gateway node
// name, the connect address, and the bound state.
func TestRenderGet_Listeners(t *testing.T) {
	t.Parallel()
	published := int32(30080)
	lb := cpclient.LoadBalancer{
		ID:            "id-1",
		Name:          "web",
		OwnerID:       "owner-1",
		Port:          80,
		Selector:      map[string]string{"app": "web"},
		PublishedPort: &published,
		Protocol:      "tcp",
		Listeners: []cpclient.Listener{
			{Node: "gw-1", Address: "10.77.0.1:30080", Bound: true, ReportedAt: "2026-07-01T10:00:00Z"},
			{Node: "gw-2", Address: "10.77.0.2:30080", Bound: false, Error: "bind: address already in use", ReportedAt: "2026-07-01T10:00:00Z"},
		},
		CreatedAt: "2026-07-01T10:00:00Z",
		UpdatedAt: "2026-07-01T10:00:00Z",
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderGet(cmd, lb); err != nil {
		t.Fatalf("renderGet: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"listeners:",
		"- node: gw-1  address: 10.77.0.1:30080  bound=true",
		"- node: gw-2  address: 10.77.0.2:30080  bound=false  error=bind: address already in use",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

// TestRenderGet_UnpublishedNoListeners asserts an unpublished load balancer (no
// listeners) renders no listeners section.
func TestRenderGet_UnpublishedNoListeners(t *testing.T) {
	t.Parallel()
	lb := cpclient.LoadBalancer{
		ID:       "id-1",
		Name:     "web",
		OwnerID:  "owner-1",
		Port:     80,
		Selector: map[string]string{"app": "web"},
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderGet(cmd, lb); err != nil {
		t.Fatalf("renderGet: %v", err)
	}
	if strings.Contains(out.String(), "listeners:") {
		t.Errorf("unpublished LB rendered a listeners section:\n%s", out.String())
	}
}

// TestRenderGet_HealthSummary locks the one-line aggregate health summary the
// text render prints when the view carries one.
func TestRenderGet_HealthSummary(t *testing.T) {
	t.Parallel()
	lb := cpclient.LoadBalancer{
		ID:       "id-1",
		Name:     "web",
		OwnerID:  "owner-1",
		Port:     80,
		Selector: map[string]string{"app": "web"},
		Health: &cpclient.LoadBalancerHealthSummary{
			Status:         "degraded",
			TargetsTotal:   2,
			TargetsHealthy: 1,
		},
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := renderGet(cmd, lb); err != nil {
		t.Fatalf("renderGet: %v", err)
	}
	if want := "health: degraded (1/2 healthy)"; !strings.Contains(out.String(), want) {
		t.Errorf("render missing %q:\n%s", want, out.String())
	}
}
