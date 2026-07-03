// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestOutputFormat verifies the shared --output parser: the base set
// (text/json/table) is always accepted, "yaml" is accepted only when the
// command opts into it via extra, an unknown format errors, and an empty
// value falls back to the command's default.
func TestOutputFormat(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		def     string
		extra   []string
		want    string
		wantErr bool
	}{
		{name: "text base", flag: "text", def: "text", want: "text"},
		{name: "json base", flag: "json", def: "text", want: "json"},
		{name: "table base", flag: "table", def: "table", want: "table"},
		{name: "empty falls back to default", flag: "", def: "table", want: "table"},
		{name: "yaml accepted when opted in", flag: "yaml", def: "text", extra: []string{"yaml"}, want: "yaml"},
		{name: "yaml rejected without opt-in", flag: "yaml", def: "text", wantErr: true},
		{name: "unknown rejected", flag: "bogus", def: "text", extra: []string{"yaml"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().StringP(flagOutput, "o", tc.def, "")
			if err := cmd.Flags().Set(flagOutput, tc.flag); err != nil {
				t.Fatalf("set flag: %v", err)
			}
			got, err := outputFormat(cmd, tc.def, tc.extra...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("outputFormat(%q, extra=%v) = %q, want error", tc.flag, tc.extra, got)
				}
				if !strings.Contains(err.Error(), "unknown format") {
					t.Errorf("outputFormat(%q) error = %v, want it to mention \"unknown format\"", tc.flag, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("outputFormat(%q, extra=%v) unexpected error: %v", tc.flag, tc.extra, err)
			}
			if got != tc.want {
				t.Errorf("outputFormat(%q, extra=%v) = %q, want %q", tc.flag, tc.extra, got, tc.want)
			}
		})
	}
}
