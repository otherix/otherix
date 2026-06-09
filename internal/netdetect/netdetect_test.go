// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netdetect

import (
	"net/netip"
	"testing"
)

func TestResolvePeerURL(t *testing.T) {
	routable := func() (netip.Addr, bool) { return netip.MustParseAddr("10.1.2.3"), true }
	noRoute := func() (netip.Addr, bool) { return netip.Addr{}, false }

	tests := []struct {
		name    string
		raw     string
		detect  func() (netip.Addr, bool)
		want    string
		wantErr bool
	}{
		{name: "auto with routable ip", raw: "auto", detect: routable, want: "https://10.1.2.3:2380"},
		{name: "empty with routable ip", raw: "", detect: routable, want: "https://10.1.2.3:2380"},
		{name: "auto with no route falls back to loopback", raw: "auto", detect: noRoute, want: "https://127.0.0.1:2380"},
		{name: "operator override returned verbatim", raw: "https://192.168.5.5:2380", detect: routable, want: "https://192.168.5.5:2380"},
		{name: "non-ipv4 detected is an error", raw: "auto", detect: func() (netip.Addr, bool) { return netip.MustParseAddr("2001:db8::1"), true }, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePeerURL(tc.raw, tc.detect)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolvePeerURL(%q) error = nil, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePeerURL(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("ResolvePeerURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
