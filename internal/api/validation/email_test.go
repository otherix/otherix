// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string // substring; empty means must succeed
	}{
		{name: "ok plain", input: "user@example.com"},
		{name: "ok with subdomain", input: "user@mail.example.co.uk"},
		{name: "ok with plus addressing", input: "user+tag@example.com"},
		{name: "empty", input: "", wantErr: "required"},
		{name: "missing at", input: "userexample.com", wantErr: "invalid email format"},
		{name: "missing local", input: "@example.com", wantErr: "invalid email format"},
		{name: "missing domain", input: "user@", wantErr: "invalid email format"},
		{name: "display name form", input: "Alice <alice@example.com>", wantErr: "bare address"},
		{name: "too long", input: strings.Repeat("a", EmailMaxLength) + "@x.io", wantErr: "exceed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEmail(tc.input)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("ValidateEmail(%q) = %v, want nil", tc.input, err)
				}
				return
			}
			if err == nil {
				t.Errorf("ValidateEmail(%q) = nil, want error containing %q", tc.input, tc.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateEmail(%q) = %v, want substring %q", tc.input, err, tc.wantErr)
			}
		})
	}
}
