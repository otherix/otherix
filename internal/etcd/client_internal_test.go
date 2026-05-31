// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"bytes"
	"errors"
	"testing"
)

func TestKey(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "single segment", parts: []string{"vms"}, want: "/otherix/vms"},
		{name: "resource + id", parts: []string{"vms", "abc"}, want: "/otherix/vms/abc"},
		{name: "index key", parts: []string{"index", "vms", "owner", "u1", "c1_id1"}, want: "/otherix/index/vms/owner/u1/c1_id1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.parts...); got != tc.want {
				t.Errorf("Key(%v) = %q, want %q", tc.parts, got, tc.want)
			}
		})
	}
}

func TestCapValue(t *testing.T) {
	if err := capValue(bytes.Repeat([]byte{'x'}, MaxValueBytes)); err != nil {
		t.Errorf("capValue(at limit) = %v, want nil", err)
	}
	err := capValue(bytes.Repeat([]byte{'x'}, MaxValueBytes+1))
	if !errors.Is(err, ErrValueTooLarge) {
		t.Errorf("capValue(over limit) = %v, want ErrValueTooLarge", err)
	}
}
