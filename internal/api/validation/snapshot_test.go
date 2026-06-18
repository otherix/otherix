// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidateSnapshotName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "s1", wantErr: false},
		{name: "default ts form", in: "snap1718000000", wantErr: false},
		{name: "dots dashes underscores", in: "daily.backup_2026-06-18", wantErr: false},
		{name: "max length ok", in: strings.Repeat("a", 255), wantErr: false},

		{name: "empty", in: "", wantErr: true},
		{name: "too long", in: strings.Repeat("a", 256), wantErr: true},
		{name: "leading dot", in: ".hidden", wantErr: true},
		{name: "leading dash", in: "-x", wantErr: true},
		// Path-traversal vectors that must NOT reach an agent filesystem path.
		{name: "slash traversal", in: "x/../../etc/passwd", wantErr: true},
		{name: "leading slash", in: "/etc/passwd", wantErr: true},
		{name: "backslash", in: `x\..\..\win`, wantErr: true},
		{name: "parent token", in: "..", wantErr: true},
		{name: "embedded slash", in: "a/b", wantErr: true},
		{name: "space", in: "a b", wantErr: true},
		{name: "null byte", in: "a\x00b", wantErr: true},
		{name: "unicode", in: "café", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSnapshotName(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSnapshotName(%q) err = %v, wantErr = %v", tt.in, err, tt.wantErr)
			}
		})
	}
}
