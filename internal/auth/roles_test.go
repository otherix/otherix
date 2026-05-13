// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

func TestRoleValid(t *testing.T) {
	tests := []struct {
		name string
		role auth.Role
		want bool
	}{
		{name: "admin", role: auth.RoleAdmin, want: true},
		{name: "operator", role: auth.RoleOperator, want: true},
		{name: "developer", role: auth.RoleDeveloper, want: true},
		{name: "viewer", role: auth.RoleViewer, want: true},
		{name: "empty", role: auth.Role(""), want: false},
		{name: "unknown", role: auth.Role("superuser"), want: false},
		{name: "wrong case", role: auth.Role("Admin"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.role.Valid(); got != tc.want {
				t.Errorf("Role(%q).Valid() = %v, want %v", string(tc.role), got, tc.want)
			}
		})
	}
}

func TestRoleStringValuesMatchSchema(t *testing.T) {
	// The constants must equal the lowercase strings stored in the
	// users.role column (CHECK constraint in 00001_init.sql).
	cases := map[auth.Role]string{
		auth.RoleAdmin:     "admin",
		auth.RoleOperator:  "operator",
		auth.RoleDeveloper: "developer",
		auth.RoleViewer:    "viewer",
	}
	for r, want := range cases {
		if string(r) != want {
			t.Errorf("Role constant = %q, want %q", string(r), want)
		}
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		in      string
		want    auth.Role
		wantErr bool
	}{
		{in: "admin", want: auth.RoleAdmin},
		{in: "operator", want: auth.RoleOperator},
		{in: "developer", want: auth.RoleDeveloper},
		{in: "viewer", want: auth.RoleViewer},
		{in: "", wantErr: true},
		{in: "Admin", wantErr: true},
		{in: "superuser", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := auth.ParseRole(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseRole(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRole(%q) err = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRole(%q) = %q, want %q", tc.in, string(got), string(tc.want))
			}
		})
	}
}
