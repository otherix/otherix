// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import "testing"

func TestValidateUsername(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "simple", in: "alice", wantErr: false},
		{name: "interior hyphen", in: "web-admin", wantErr: false},
		{name: "digits and hyphens", in: "ci-bot-01", wantErr: false},
		{name: "min length", in: "abc", wantErr: false},
		{name: "max length", in: "a234567890123456789012345678901z", wantErr: false}, // 32
		{name: "empty", in: "", wantErr: true},
		{name: "too short", in: "ab", wantErr: true},
		{name: "too long", in: "a2345678901234567890123456789012x", wantErr: true}, // 33
		{name: "uppercase", in: "Alice", wantErr: true},
		{name: "leading hyphen", in: "-bot", wantErr: true},
		{name: "trailing hyphen", in: "bot-", wantErr: true},
		{name: "underscore", in: "a_b", wantErr: true},
		{name: "dot", in: "a.b", wantErr: true},
		{name: "space", in: "a b", wantErr: true},
		{name: "at sign", in: "a@b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateUsername(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateUsername(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}
