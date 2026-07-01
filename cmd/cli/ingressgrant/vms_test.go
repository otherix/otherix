// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrant

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestParseVMScopePorts(t *testing.T) {
	got, err := parseVMScope([]string{"web:22", "db:5432,8080"}, "ubuntu")
	if err != nil {
		t.Fatalf("parseVMScope: %v", err)
	}
	want := []cpclient.IngressGrantVM{
		{VMName: "web", Ports: []int{22}, Login: "ubuntu"},
		{VMName: "db", Ports: []int{5432, 8080}, Login: "ubuntu"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("parseVMScope mismatch (-want +got):\n%s", diff)
	}
}

func TestParseVMScopeRejectsMalformed(t *testing.T) {
	// Single-entry malformed inputs each error.
	for name, entry := range map[string]string{
		"no port":      "web",
		"empty ports":  "web:",
		"non-integer":  "web:http",
		"out of range": "web:70000",
		"zero port":    "web:0",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseVMScope([]string{entry}, "root"); err == nil {
				t.Errorf("parseVMScope(%q) = nil error, want error", entry)
			}
		})
	}

	// A duplicate VM name across entries is rejected.
	if _, err := parseVMScope([]string{"web:22", "web:80"}, "root"); err == nil {
		t.Errorf("parseVMScope duplicate vm = nil error, want error")
	}

	// No entries at all is rejected.
	if _, err := parseVMScope(nil, "root"); err == nil {
		t.Errorf("parseVMScope(nil) = nil error, want error")
	}
}
