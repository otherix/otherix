// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"net/netip"
	"testing"
)

func TestSourceIPAllowed(t *testing.T) {
	tests := []struct {
		name   string
		cidrs  []string
		client string
		want   bool
	}{
		{"nil open", nil, "10.1.2.3", true},
		{"empty open", []string{}, "10.1.2.3", true},
		{"in range", []string{"10.0.0.0/8"}, "10.1.2.3", true},
		{"out of range", []string{"10.0.0.0/8"}, "192.168.1.1", false},
		{"bare ip match", []string{"203.0.113.5/32"}, "203.0.113.5", true},
		{"bare ip miss", []string{"203.0.113.5/32"}, "203.0.113.6", false},
		{"malformed entry skipped", []string{"garbage", "10.0.0.0/8"}, "10.1.2.3", true},
		{"malformed only denies", []string{"garbage"}, "10.1.2.3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceIPAllowed(tt.cidrs, netip.MustParseAddr(tt.client)); got != tt.want {
				t.Errorf("sourceIPAllowed(%v, %s) = %v, want %v", tt.cidrs, tt.client, got, tt.want)
			}
		})
	}
	if sourceIPAllowed([]string{"10.0.0.0/8"}, netip.Addr{}) {
		t.Errorf("sourceIPAllowed with invalid client = true, want false")
	}
}
