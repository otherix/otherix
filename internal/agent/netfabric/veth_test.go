// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import "testing"

func TestGatewayVethNames(t *testing.T) {
	if got := GatewayVethHostName(4200); got != "otvg4200" {
		t.Errorf("GatewayVethHostName(4200) = %q, want %q", got, "otvg4200")
	}
	if got := GatewayVethPeerName(4200); got != "otvgp4200" {
		t.Errorf("GatewayVethPeerName(4200) = %q, want %q", got, "otvgp4200")
	}
}

// The kernel IFNAMSIZ is 16, so a name must stay <= 15 bytes. The largest
// 24-bit VNI (16777215) must still yield in-bound host and peer names.
func TestGatewayVethNamesWithinIfnameSize(t *testing.T) {
	const maxVNI uint32 = 0xFFFFFF
	for _, name := range []string{GatewayVethHostName(maxVNI), GatewayVethPeerName(maxVNI)} {
		if len(name) > 15 {
			t.Errorf("veth name %q = %d bytes, want <= 15 (IFNAMSIZ-1)", name, len(name))
		}
	}
}

func TestFakeFabricRecordsVethCalls(t *testing.T) {
	f := &FakeFabric{}
	cfg := VethConfig{HostName: "otvg7", PeerName: "otvgp7", Bridge: "otvb7"}
	if err := f.EnsureVeth(cfg); err != nil {
		t.Fatalf("EnsureVeth() error = %v", err)
	}
	if err := f.RemoveVeth("otvg7"); err != nil {
		t.Fatalf("RemoveVeth() error = %v", err)
	}
	if len(f.EnsureVethCalls) != 1 || f.EnsureVethCalls[0].HostName != "otvg7" {
		t.Errorf("EnsureVethCalls = %+v, want one call for otvg7", f.EnsureVethCalls)
	}
	if len(f.RemoveVethCalls) != 1 || f.RemoveVethCalls[0] != "otvg7" {
		t.Errorf("RemoveVethCalls = %+v, want [otvg7]", f.RemoveVethCalls)
	}
}
