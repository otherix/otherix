// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidateArchitecture(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "amd64", input: "amd64", wantErr: false},
		{name: "arm64", input: "arm64", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "uppercase", input: "AMD64", wantErr: true},
		{name: "x86_64", input: "x86_64", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateArchitecture(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateArchitecture(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateFirmwareType(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "uefi", input: "uefi", wantErr: false},
		{name: "bios", input: "bios", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "uppercase", input: "UEFI", wantErr: true},
		{name: "unknown", input: "coreboot", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFirmwareType(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateFirmwareType(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateFirmwareName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "ascii", input: "OVMF-amd64", wantErr: false},
		{name: "with-spaces", input: "OVMF Code Image", wantErr: false},
		{name: "unicode", input: "прошивка", wantErr: false},
		{name: "max len", input: strings.Repeat("a", FirmwareNameMaxLength), wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "leading space", input: " name", wantErr: true},
		{name: "trailing space", input: "name ", wantErr: true},
		{name: "too long", input: strings.Repeat("a", FirmwareNameMaxLength+1), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFirmwareName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateFirmwareName(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateFirmwareVersion(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty (treated as null)", input: "", wantErr: false},
		{name: "semver", input: "4.2024.08-1", wantErr: false},
		{name: "with letters", input: "edk2-stable202311", wantErr: false},
		{name: "max len", input: strings.Repeat("a", FirmwareVersionMaxLength), wantErr: false},
		{name: "too long", input: strings.Repeat("a", FirmwareVersionMaxLength+1), wantErr: true},
		{name: "with NUL", input: "v1\x00bad", wantErr: true},
		{name: "with newline", input: "v1\nbad", wantErr: true},
		{name: "with tab", input: "v1\tbad", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFirmwareVersion(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateFirmwareVersion(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
