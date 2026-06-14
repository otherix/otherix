// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import "testing"

func TestShortID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"truncates a uuid to the 8-char first segment", "abcdef0123456789-0000-0000", "abcdef01"},
		{"exactly 8 is unchanged", "abcdef01", "abcdef01"},
		{"shorter than 8 is unchanged", "abc", "abc"},
		{"empty is unchanged", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortID(tc.in); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNodeLabel(t *testing.T) {
	name := "node-a"
	empty := ""
	id := "node-a-id"
	cases := []struct {
		name     string
		nodeName *string
		nodeID   *string
		want     string
	}{
		{"name present wins", &name, &id, "node-a"},
		{"nil name falls back to id", nil, &id, "node-a-id"},
		{"empty name falls back to id", &empty, &id, "node-a-id"},
		{"both nil renders dash", nil, nil, "-"},
		{"name nil and id empty renders dash", nil, &empty, "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeLabel(tc.nodeName, tc.nodeID); got != tc.want {
				t.Errorf("nodeLabel(%v, %v) = %q, want %q", tc.nodeName, tc.nodeID, got, tc.want)
			}
		})
	}
}
