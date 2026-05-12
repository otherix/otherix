// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import "testing"

func TestValidateNetworkType(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "bridge", input: "bridge", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown", input: "nat", wantErr: true},
		{name: "uppercase", input: "Bridge", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetworkType(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNetworkType(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateBridgeName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "br0", input: "br0", wantErr: false},
		{name: "br-lan", input: "br-lan", wantErr: false},
		{name: "br_main", input: "br_main", wantErr: false},
		{name: "max len 15", input: "br" + "0123456789abc", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "leading digit", input: "0br", wantErr: true},
		{name: "too long", input: "br" + "0123456789abcd", wantErr: true},
		{name: "invalid char", input: "br/0", wantErr: true},
		{name: "space", input: "br 0", wantErr: true},
		{name: "leading dash", input: "-br", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBridgeName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateBridgeName(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateVLANTag(t *testing.T) {
	cases := []struct {
		name    string
		input   int
		wantErr bool
	}{
		{name: "min", input: 1, wantErr: false},
		{name: "max", input: 4094, wantErr: false},
		{name: "typical", input: 100, wantErr: false},
		{name: "zero reserved", input: 0, wantErr: true},
		{name: "4095 reserved", input: 4095, wantErr: true},
		{name: "negative", input: -1, wantErr: true},
		{name: "huge", input: 8192, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVLANTag(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateVLANTag(%d) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateMTU(t *testing.T) {
	cases := []struct {
		name    string
		input   int
		wantErr bool
	}{
		{name: "min", input: MinMTU, wantErr: false},
		{name: "default", input: DefaultMTU, wantErr: false},
		{name: "vpn overlay", input: 1450, wantErr: false},
		{name: "jumbo", input: 9000, wantErr: false},
		{name: "max", input: MaxMTU, wantErr: false},
		{name: "below min", input: 67, wantErr: true},
		{name: "above max", input: MaxMTU + 1, wantErr: true},
		{name: "zero", input: 0, wantErr: true},
		{name: "negative", input: -1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMTU(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateMTU(%d) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
