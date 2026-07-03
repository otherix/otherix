// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNodeUnmarshalMigratesLegacyKind(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"legacy_gateway", `{"Name":"gw","Kind":"gateway"}`, true},
		{"legacy_node", `{"Name":"n","Kind":"node"}`, false},
		{"legacy_empty", `{"Name":"n"}`, false},
		{"new_gateway_bit", `{"Name":"gw","GatewayRole":true}`, true},
		{"new_hypervisor_bit", `{"Name":"n","GatewayRole":false}`, false},
	}
	for _, tc := range cases {
		var n Node
		if err := json.Unmarshal([]byte(tc.raw), &n); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.name, err)
		}
		if n.GatewayRole != tc.want {
			t.Errorf("Unmarshal(%s).GatewayRole = %v, want %v", tc.name, n.GatewayRole, tc.want)
		}
	}
}

func TestNodeHasRoleAndRoles(t *testing.T) {
	gw := Node{GatewayRole: true}
	hv := Node{GatewayRole: false}
	if !gw.HasRole(NodeRoleGateway) || gw.HasRole(NodeRoleHypervisor) {
		t.Errorf("gateway node HasRole gateway/hypervisor = %v/%v, want true/false",
			gw.HasRole(NodeRoleGateway), gw.HasRole(NodeRoleHypervisor))
	}
	if !hv.HasRole(NodeRoleHypervisor) || hv.HasRole(NodeRoleGateway) {
		t.Errorf("hypervisor node HasRole hypervisor/gateway = %v/%v, want true/false",
			hv.HasRole(NodeRoleHypervisor), hv.HasRole(NodeRoleGateway))
	}
	if hv.HasRole("bogus") {
		t.Errorf("HasRole(bogus) = true, want false")
	}
	if diff := cmp.Diff([]string{NodeRoleGateway}, gw.Roles()); diff != "" {
		t.Errorf("gateway Roles() mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{NodeRoleHypervisor}, hv.Roles()); diff != "" {
		t.Errorf("hypervisor Roles() mismatch (-want +got):\n%s", diff)
	}
}

func TestEffectiveRoles(t *testing.T) {
	tests := []struct {
		name        string
		gatewayRole bool
		ownsPool    bool
		want        []string
	}{
		{name: "neither", gatewayRole: false, ownsPool: false, want: []string{}},
		{name: "gateway only", gatewayRole: true, ownsPool: false, want: []string{NodeRoleGateway}},
		{name: "hypervisor only", gatewayRole: false, ownsPool: true, want: []string{NodeRoleHypervisor}},
		{name: "co-located", gatewayRole: true, ownsPool: true, want: []string{NodeRoleHypervisor, NodeRoleGateway}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveRoles(tt.gatewayRole, tt.ownsPool)
			if got == nil {
				t.Fatalf("EffectiveRoles(%v, %v) = nil, want non-nil (must marshal to [])", tt.gatewayRole, tt.ownsPool)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("EffectiveRoles(%v, %v) mismatch (-want +got):\n%s", tt.gatewayRole, tt.ownsPool, diff)
			}
		})
	}
}
