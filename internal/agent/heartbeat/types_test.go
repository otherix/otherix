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
