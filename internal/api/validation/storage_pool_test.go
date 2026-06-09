// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidateStoragePoolType(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "local_dir", input: "local_dir", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown", input: "ceph_rbd", wantErr: true},
		{name: "uppercase", input: "Local_Dir", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStoragePoolType(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateStoragePoolType(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateStoragePoolName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "ascii", input: "default", wantErr: false},
		{name: "with-dash", input: "ssd-fast", wantErr: false},
		{name: "with-underscore", input: "pool_a", wantErr: false},
		{name: "unicode", input: "хранилище", wantErr: false},
		{name: "max len", input: strings.Repeat("a", StoragePoolNameMaxLength), wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "leading space", input: " name", wantErr: true},
		{name: "trailing space", input: "name ", wantErr: true},
		{name: "too long", input: strings.Repeat("a", StoragePoolNameMaxLength+1), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStoragePoolName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateStoragePoolName(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidatePoolPath(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "absolute", input: "/var/lib/otherix/pools/default", wantErr: false},
		{name: "root", input: "/", wantErr: false},
		{name: "trailing slash", input: "/var/lib/", wantErr: false},
		{name: "with-dotdot", input: "/var/lib/../foo", wantErr: false},
		{name: "max len", input: "/" + strings.Repeat("a", PoolPathMaxLength-1), wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "relative", input: "var/lib", wantErr: true},
		{name: "leading dot", input: "./var/lib", wantErr: true},
		{name: "tilde", input: "~/pools", wantErr: true},
		{name: "too long", input: "/" + strings.Repeat("a", PoolPathMaxLength), wantErr: true},
		{name: "with NUL", input: "/var/\x00/lib", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePoolPath(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidatePoolPath(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
