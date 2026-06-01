// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import "testing"

func TestValidateNICModel(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty allowed", input: "", wantErr: false},
		{name: "virtio", input: "virtio", wantErr: false},
		{name: "e1000", input: "e1000", wantErr: false},
		{name: "e1000e", input: "e1000e", wantErr: false},
		{name: "rtl8139", input: "rtl8139", wantErr: false},
		{name: "bogus", input: "bogus", wantErr: true},
		{name: "vmxnet3 unsupported", input: "vmxnet3", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNICModel(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNICModel(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateNICMAC(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "eui48 ok", input: "52:54:00:12:34:56", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "garbage", input: "not-a-mac", wantErr: true},
		{name: "eui64 rejected", input: "52:54:00:12:34:56:78:90", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNICMAC(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNICMAC(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDeviceOrder(t *testing.T) {
	cases := []struct {
		name    string
		input   int
		wantErr bool
	}{
		{name: "zero ok", input: 0, wantErr: false},
		{name: "max ok", input: 15, wantErr: false},
		{name: "mid ok", input: 7, wantErr: false},
		{name: "negative", input: -1, wantErr: true},
		{name: "too large", input: 16, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDeviceOrder(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateDeviceOrder(%d) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
