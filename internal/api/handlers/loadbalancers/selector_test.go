// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import "testing"

func TestSelectorMatches(t *testing.T) {
	cases := []struct {
		name     string
		selector map[string]string
		labels   string // vm.Labels JSON
		want     bool
	}{
		{"single match", map[string]string{"app": "web"}, `{"app":"web","tier":"fe"}`, true},
		{"and all match", map[string]string{"app": "web", "tier": "fe"}, `{"app":"web","tier":"fe"}`, true},
		{"one mismatch", map[string]string{"app": "web", "tier": "be"}, `{"app":"web","tier":"fe"}`, false},
		{"missing key", map[string]string{"zone": "a"}, `{"app":"web"}`, false},
		{"empty labels", map[string]string{"app": "web"}, `{}`, false},
		{"malformed labels", map[string]string{"app": "web"}, `not json`, false},
		{"empty selector", map[string]string{}, `{"app":"web"}`, false},
	}
	for _, c := range cases {
		got := selectorMatches(c.selector, []byte(c.labels))
		if got != c.want {
			t.Errorf("%s: selectorMatches = %v, want %v", c.name, got, c.want)
		}
	}
}
