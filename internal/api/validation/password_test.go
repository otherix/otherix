// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "ok min length", input: strings.Repeat("a", PasswordMinLength)},
		{name: "ok max length", input: strings.Repeat("a", PasswordMaxLength)},
		{name: "ok with unicode", input: "пароль-длинный-достаточно"},
		{name: "empty", input: "", wantErr: "required"},
		{name: "one short", input: strings.Repeat("a", PasswordMinLength-1), wantErr: "at least"},
		{name: "one long", input: strings.Repeat("a", PasswordMaxLength+1), wantErr: "exceed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.input)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("ValidatePassword(len=%d) = %v, want nil", len(tc.input), err)
				}
				return
			}
			if err == nil {
				t.Errorf("ValidatePassword(len=%d) = nil, want error containing %q", len(tc.input), tc.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidatePassword(len=%d) = %v, want substring %q", len(tc.input), err, tc.wantErr)
			}
		})
	}
}
