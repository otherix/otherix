// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"net/netip"
	"testing"
)

func TestSourceIPAllows(t *testing.T) {
	cidr := "203.0.113.0/24"
	bare := "198.51.100.7"
	cases := []struct {
		name   string
		pin    *string
		client string
		want   bool
	}{
		{"nil pin allows anything", nil, "8.8.8.8", true},
		{"cidr in range", &cidr, "203.0.113.5", true},
		{"cidr out of range", &cidr, "203.0.114.5", false},
		{"bare exact match", &bare, "198.51.100.7", true},
		{"bare mismatch", &bare, "198.51.100.8", false},
	}
	for _, tc := range cases {
		got := SourceIPAllows(tc.pin, netip.MustParseAddr(tc.client))
		if got != tc.want {
			t.Errorf("%s: SourceIPAllows(%v,%s) = %v, want %v", tc.name, tc.pin, tc.client, got, tc.want)
		}
	}
}
