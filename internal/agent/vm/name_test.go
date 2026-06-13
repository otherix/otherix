// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"strings"
	"testing"
)

func TestValidVMName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"normal label", "web-01", true},
		{"single char", "a", true},
		{"digits only", "12345", true},
		{"max 63-char label", strings.Repeat("a", 63), true},
		{"empty", "", false},
		{"newline injection", "web\n01", false},
		{"leading hyphen", "-web", false},
		{"trailing hyphen", "web-", false},
		{"uppercase", "Web", false},
		{"comma", "a,b", false},
		{"space", "a b", false},
		{"underscore", "a_b", false},
		{"64-char label", strings.Repeat("a", 64), false},
		{"dot", "web.01", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validVMName(tc.in); got != tc.want {
				t.Errorf("validVMName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
