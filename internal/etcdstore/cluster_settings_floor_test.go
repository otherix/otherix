// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import "testing"

// TestUnderlayMTUBelowFloor exercises the read-side floor predicate that backs
// the boot-time WARN. The 1390 floor was 1280 before the floor was raised, so a
// cluster seeded under an older binary can carry a value in [1280,1389] that
// derives a sub-1280 overlay MTU (below the RFC 8200 IPv6 minimum). The predicate
// must flag strictly-below-floor seeds and pass the default 1500 / the floor
// itself / any valid seed untouched.
func TestUnderlayMTUBelowFloor(t *testing.T) {
	cases := []struct {
		name string
		mtu  int32
		want bool
	}{
		{name: "legacy 1280 seed below the floor", mtu: 1280, want: true},
		{name: "one below the floor", mtu: MinUnderlayMTU - 1, want: true},
		{name: "exactly the floor", mtu: MinUnderlayMTU, want: false},
		{name: "default 1500", mtu: 1500, want: false},
		{name: "jumbo 9000", mtu: 9000, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnderlayMTUBelowFloor(tc.mtu); got != tc.want {
				t.Errorf("UnderlayMTUBelowFloor(%d) = %v, want %v", tc.mtu, got, tc.want)
			}
		})
	}
}
