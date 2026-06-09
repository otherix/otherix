// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestResponseDecodeDeclaredNetworks verifies a HeartbeatResponse JSON body
// with declared_networks decodes into the Response struct, covering a nat
// network (subnet + gateway populated) and a plain managed bridge (subnet /
// gateway null).
func TestResponseDecodeDeclaredNetworks(t *testing.T) {
	subnet := "10.10.0.0/24"
	gateway := "10.10.0.1"
	body := `{
		"received_at": "2026-06-01T00:00:00Z",
		"declared_pools": [],
		"declared_vms": [],
		"declared_networks": [
			{
				"id": "11111111-1111-1111-1111-111111111111",
				"name": "vmnat",
				"type": "bridge",
				"managed": true,
				"egress": "nat",
				"bridge_name": "otx-vmnat",
				"mtu": 1500,
				"subnet": "10.10.0.0/24",
				"gateway": "10.10.0.1"
			},
			{
				"id": "22222222-2222-2222-2222-222222222222",
				"name": "plain",
				"type": "bridge",
				"managed": true,
				"egress": "none",
				"bridge_name": "br0",
				"mtu": 9000,
				"subnet": null,
				"gateway": null
			}
		]
	}`

	var got Response
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Unmarshal heartbeat response: %v", err)
	}

	want := []DeclaredNetwork{
		{
			ID:         "11111111-1111-1111-1111-111111111111",
			Name:       "vmnat",
			Type:       "bridge",
			Managed:    true,
			Egress:     "nat",
			BridgeName: "otx-vmnat",
			Mtu:        1500,
			Subnet:     &subnet,
			Gateway:    &gateway,
		},
		{
			ID:         "22222222-2222-2222-2222-222222222222",
			Name:       "plain",
			Type:       "bridge",
			Managed:    true,
			Egress:     "none",
			BridgeName: "br0",
			Mtu:        9000,
			Subnet:     nil,
			Gateway:    nil,
		},
	}
	if diff := cmp.Diff(want, got.DeclaredNetworks); diff != "" {
		t.Errorf("DeclaredNetworks mismatch (-want +got):\n%s", diff)
	}
}

// TestResponseDecodeDeclaredNetworkDHCP verifies a declared_networks entry
// carrying dhcp + reservations decodes into DhcpEnabled + the Reservations
// slice. The response decoder uses DisallowUnknownFields, so this also guards
// that the new wire fields exist on the struct (a missing field would fail the
// decode, not silently drop).
func TestResponseDecodeDeclaredNetworkDHCP(t *testing.T) {
	body := `{
		"received_at": "2026-06-01T00:00:00Z",
		"declared_pools": [],
		"declared_vms": [],
		"declared_networks": [
			{
				"id": "33333333-3333-3333-3333-333333333333",
				"name": "vmdhcp",
				"type": "bridge",
				"managed": true,
				"egress": "nat",
				"bridge_name": "otx-vmdhcp",
				"mtu": 1500,
				"subnet": "10.62.0.0/24",
				"gateway": "10.62.0.1",
				"vni": null,
				"dhcp": true,
				"reservations": [{"mac": "52:54:00:aa:bb:cc", "ip": "10.62.0.5"}]
			}
		]
	}`

	var got Response
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Unmarshal heartbeat response: %v", err)
	}
	if len(got.DeclaredNetworks) != 1 {
		t.Fatalf("DeclaredNetworks len = %d, want 1", len(got.DeclaredNetworks))
	}
	dn := got.DeclaredNetworks[0]
	if !dn.DhcpEnabled {
		t.Errorf("DhcpEnabled = false, want true")
	}
	want := []DhcpReservation{{MAC: "52:54:00:aa:bb:cc", IP: "10.62.0.5"}}
	if diff := cmp.Diff(want, dn.Reservations); diff != "" {
		t.Errorf("Reservations mismatch (-want +got):\n%s", diff)
	}
}

func TestResponseDecodeDeclaredFDB(t *testing.T) {
	raw := `{"received_at":"t","declared_pools":[],"declared_vms":[],"declared_networks":[],
	         "declared_wireguard_peers":[],"self_overlay_ip":null,
	         "declared_fdb":[{"vni":1000,"mac":"52:54:00:aa:bb:cc","vtep_ip":"10.42.0.2"},
	                         {"vni":1000,"mac":"00:00:00:00:00:00","vtep_ip":"10.42.0.2"}]}`
	var got Response
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []DeclaredFDBEntry{
		{VNI: 1000, MAC: "52:54:00:aa:bb:cc", VtepIP: "10.42.0.2"},
		{VNI: 1000, MAC: "00:00:00:00:00:00", VtepIP: "10.42.0.2"},
	}
	if diff := cmp.Diff(want, got.DeclaredFDB); diff != "" {
		t.Errorf("DeclaredFDB mismatch (-want +got):\n%s", diff)
	}
}

func TestResponseDecodeOtwg0MTU(t *testing.T) {
	raw := `{"received_at":"t","declared_pools":[],"declared_vms":[],"declared_networks":[],
	         "declared_wireguard_peers":[],"self_overlay_ip":null,"declared_fdb":[],"otwg0_mtu":1440}`
	var got Response
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Otwg0MTU == nil || *got.Otwg0MTU != 1440 {
		t.Errorf("Otwg0MTU = %v, want 1440", got.Otwg0MTU)
	}
}
