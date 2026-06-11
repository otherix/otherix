// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
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
		{name: "with-dash-digit", input: "pool-1", wantErr: false},
		{name: "with-underscore", input: "pool_a", wantErr: false},
		{name: "with-underscore-word", input: "my_pool", wantErr: false},
		{name: "with-dot", input: "pool.2", wantErr: false},
		{name: "uppercase", input: "Pool1", wantErr: false},
		{name: "max len", input: strings.Repeat("a", StoragePoolNameMaxLength), wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "leading space", input: " name", wantErr: true},
		{name: "trailing space", input: "name ", wantErr: true},
		{name: "embedded space", input: "a b", wantErr: true},
		{name: "unicode", input: "хранилище", wantErr: true},
		{name: "slash", input: "foo/evil", wantErr: true},
		{name: "dot", input: ".", wantErr: true},
		{name: "dotdot", input: "..", wantErr: true},
		{name: "leading slash", input: "/abs", wantErr: true},
		{name: "leading dot", input: ".hidden", wantErr: true},
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
		{name: "clean pool path", input: "/var/lib/otherix/pools/data", wantErr: false},
		{name: "root", input: "/", wantErr: false},
		{name: "trailing slash", input: "/var/lib/", wantErr: false},
		{name: "dotdot inside filename", input: "/var/lib/otherix/pools/a..b", wantErr: false},
		{name: "max len", input: "/" + strings.Repeat("a", PoolPathMaxLength-1), wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "relative", input: "var/lib", wantErr: true},
		{name: "leading dot", input: "./var/lib", wantErr: true},
		{name: "tilde", input: "~/pools", wantErr: true},
		{name: "dotdot segment", input: "/var/lib/../foo", wantErr: true},
		{name: "dotdot traversal", input: "/var/lib/otherix/pools/../../../etc", wantErr: true},
		{name: "trailing dotdot", input: "/var/lib/otherix/pools/..", wantErr: true},
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

func TestValidatePoolPathAgainstAllowlist(t *testing.T) {
	prefixes := []string{"/var/lib/otherix/pools/"}
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "under prefix", path: "/var/lib/otherix/pools/data", wantErr: false},
		{name: "prefix itself no trailing slash", path: "/var/lib/otherix/pools", wantErr: false},
		{name: "prefix itself trailing slash", path: "/var/lib/otherix/pools/", wantErr: false},
		{name: "dotdot inside filename", path: "/var/lib/otherix/pools/a..b", wantErr: false},
		{name: "outside prefix", path: "/etc/passwd", wantErr: true},
		{name: "sibling prefix", path: "/var/lib/otherix/pools-evil/data", wantErr: true},
		{name: "dotdot traversal escape", path: "/var/lib/otherix/pools/../../../etc", wantErr: true},
		{name: "dotdot to prefix parent", path: "/var/lib/otherix/pools/..", wantErr: true},
		{name: "dotdot resolving back inside", path: "/var/lib/otherix/pools/data/..", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePoolPathAgainstAllowlist(tc.path, prefixes)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidatePoolPathAgainstAllowlist(%q, %v) err = %v, wantErr = %v", tc.path, prefixes, err, tc.wantErr)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrPoolPathNotAllowed) {
				t.Errorf("ValidatePoolPathAgainstAllowlist(%q, %v) err = %v, want ErrPoolPathNotAllowed", tc.path, prefixes, err)
			}
		})
	}
}
