// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// TestOwnerLabel covers the owner-identifier fallback: the display_name
// is used when set, otherwise the email (so a user without a display
// name - e.g. the bootstrap admin - still resolves to a non-empty
// label rather than a blank owner field).
func TestOwnerLabel(t *testing.T) {
	cases := []struct {
		name string
		user store.User
		want string
	}{
		{
			name: "display_name set",
			user: store.User{DisplayName: "Ada Lovelace", Email: "ada@example.test"},
			want: "Ada Lovelace",
		},
		{
			name: "empty display_name falls back to email",
			user: store.User{DisplayName: "", Email: "admin@otherix.local"},
			want: "admin@otherix.local",
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
