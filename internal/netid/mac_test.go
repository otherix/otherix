// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netid

import "testing"

// TestGenerateLocalMAC asserts the minted MAC carries the QEMU 52:54:00 OUI,
// is locally administered + unicast, and varies across calls.
func TestGenerateLocalMAC(t *testing.T) {
	mac, err := GenerateLocalMAC()
	if err != nil {
		t.Fatalf("GenerateLocalMAC: %v", err)
	}
	if len(mac) != 6 {
		t.Fatalf("len(mac) = %d, want 6", len(mac))
	}
	if mac[0] != 0x52 || mac[1] != 0x54 || mac[2] != 0x00 {
		t.Errorf("mac OUI = %02x:%02x:%02x, want 52:54:00", mac[0], mac[1], mac[2])
	}
	// Locally administered (bit 1 set) + unicast (bit 0 clear) in the first octet.
	if mac[0]&0x02 == 0 {
		t.Errorf("mac first octet %02x is not locally administered", mac[0])
	}
	if mac[0]&0x01 != 0 {
		t.Errorf("mac first octet %02x is not unicast", mac[0])
	}
	other, err := GenerateLocalMAC()
	if err != nil {
		t.Fatalf("GenerateLocalMAC (2nd): %v", err)
	}
	if mac.String() == other.String() {
		t.Errorf("two generated MACs collided: %s", mac)
	}
}
