// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import "testing"

// TestIsUUIDPrefix covers the short-id charset: a prefix of a canonical UUID
// string is hex with hyphens at positions 8/13/18/23, so the CLI's 12-char
// short id (which includes the first hyphen) must round-trip back to the
// resolver, while non-UUID-shaped input is rejected.
func TestIsUUIDPrefix(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"7268d313", true},                             // first segment, pure hex
		{"7268d313-b16", true},                         // the CLI's 12-char short id (with hyphen)
		{"7268d313-b16b", true},                        // through the second hyphen position
		{"7268d313-b16b-4f83-a533-3053e17bccc4", true}, // full canonical uuid
		{"7268d313b16b", false},                        // hyphen missing at position 8
		{"7268d313-", true},                            // hyphen exactly at position 8
		{"not-a-uuid", false},                          // hyphen at a non-hyphen position
		{"7268D313", false},                            // uppercase (caller lowercases before this)
		{"7268g313", false},                            // non-hex digit
		{"", false},                                    // empty
		{"7268d313-b16b-4f83-a533-3053e17bccc4-extra", false}, // longer than a uuid
	}
	for _, tc := range tests {
		if got := isUUIDPrefix(tc.in); got != tc.want {
			t.Errorf("isUUIDPrefix(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
