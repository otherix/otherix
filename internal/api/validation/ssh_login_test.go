// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"strings"
	"testing"
)

func TestValidateSSHLogin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"dev", "dev", true},
		{"ubuntu", "ubuntu", true},
		{"root", "root", true}, // charset-valid; the guest sshd decides
		{"_svc", "_svc", true},
		{"a-b_c0", "a-b_c0", true},
		{"", "", false},
		{"dev;rm -rf", "", false},
		{"../x", "", false},
		{"Dev", "", false},                   // leading uppercase rejected
		{"0day", "", false},                  // leading digit rejected
		{"foo bar", "", false},               // space rejected
		{"foo$bar", "", false},               // shell metachar rejected
		{strings.Repeat("a", 33), "", false}, // over length cap
	}
	for _, c := range cases {
		got, err := ValidateSSHLogin(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ValidateSSHLogin(%q) = (%q, %v), want (%q, ok=%v)", c.in, got, err, c.want, c.ok)
		}
	}
}
