// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestParseSelector locks the selector parsing contract: a valid
// multi-pair string maps every entry, and each malformed shape (empty
// value, bare key, duplicate key, empty input) is rejected.
func TestParseSelector(t *testing.T) {
	t.Parallel()

	t.Run("valid multi-pair", func(t *testing.T) {
		t.Parallel()
		got, err := parseSelector("app=web,tier=fe")
		if err != nil {
			t.Fatalf("parseSelector: %v", err)
		}
		want := map[string]string{"app": "web", "tier": "fe"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("parseSelector mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("single pair with surrounding whitespace", func(t *testing.T) {
		t.Parallel()
		got, err := parseSelector(" app=web ")
		if err != nil {
			t.Fatalf("parseSelector: %v", err)
		}
		want := map[string]string{"app": "web"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("parseSelector mismatch (-want +got):\n%s", diff)
		}
	})

	rejected := []struct {
		name string
		in   string
	}{
		{"empty value", "app="},
		{"bare key", "app"},
		{"empty key", "=web"},
		{"duplicate key", "app=web,app=api"},
		{"empty input", ""},
		{"only commas", ",,"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseSelector(tc.in); err == nil {
				t.Errorf("parseSelector(%q) = nil error, want rejection", tc.in)
			}
		})
	}
}
