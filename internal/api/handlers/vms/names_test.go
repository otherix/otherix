// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// TestOwnerLabel covers the owner-identifier fallback: the display_name
// is used when set, otherwise the username (always present, so a user
// without a display name - e.g. the bootstrap admin - still resolves to
// a non-empty label rather than a blank owner field). Email is optional
// and is never used as a label, so it must not affect the result.
func TestOwnerLabel(t *testing.T) {
	cases := []struct {
		name string
		user store.User
		want string
	}{
		{
			name: "display_name set",
			user: store.User{DisplayName: "Ada Lovelace", Username: "ada", Email: "ada@example.test"},
			want: "Ada Lovelace",
		},
		{
			name: "empty display_name falls back to username",
			user: store.User{DisplayName: "", Username: "admin", Email: "admin@otherix.local"},
			want: "admin",
		},
		{
			name: "no display_name and no email falls back to username",
			user: store.User{DisplayName: "", Username: "admin", Email: ""},
			want: "admin",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerLabel(tc.user); got != tc.want {
				t.Errorf("ownerLabel(%+v) = %q, want %q", tc.user, got, tc.want)
			}
		})
	}
}
