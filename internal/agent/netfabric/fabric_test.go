// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"testing"

	"github.com/google/uuid"
)

func TestNICTapName(t *testing.T) {
	id := uuid.MustParse("0123456f-89ab-cdef-0123-456789abcdef")
	n := NIC{ID: id}
	got := n.TapName()
	want := "ot0123456f89ab"
	if got != want {
		t.Errorf("NIC{ID: %s}.TapName() = %q, want %q", id, got, want)
	}
	if len(got) != 14 {
		t.Errorf("len(TapName()) = %d, want 14", len(got))
	}
}

func TestValidateMAC(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "valid locally administered", in: "52:54:00:12:34:56", wantErr: false},
		{name: "valid uppercase", in: "AA:BB:CC:DD:EE:FF", wantErr: false},
		{name: "empty", in: "", wantErr: true},
		{name: "garbage", in: "not-a-mac", wantErr: true},
		{name: "eight octet eui64", in: "52:54:00:12:34:56:78:9a", wantErr: true},
		{name: "too short", in: "52:54:00", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMAC(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateMAC(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateModel(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "empty defaults later", in: "", wantErr: false},
		{name: "virtio", in: "virtio", wantErr: false},
		{name: "e1000", in: "e1000", wantErr: false},
		{name: "e1000e", in: "e1000e", wantErr: false},
		{name: "rtl8139", in: "rtl8139", wantErr: false},
		{name: "unknown", in: "vmxnet3", wantErr: true},
		{name: "wrong case", in: "Virtio", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateModel(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateModel(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}
