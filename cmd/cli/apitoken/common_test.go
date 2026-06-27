// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken

import (
	"testing"
	"time"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestParseTTL(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"90d", 90 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"30d12h", 30*24*time.Hour + 12*time.Hour, false},
		{"45m", 45 * time.Minute, false},
		{"0", 0, true},
		{"0d", 0, true},
		{"-5d", 0, true},
		{"", 0, true},
		{"abc", 0, true},
		{"d", 0, true},
		{"12h30d", 0, true},                       // day component must come first (deliberate reject)
		{"36500d", 36500 * 24 * time.Hour, false}, // at the maxTTLDays cap, accepted
		{"36501d", 0, true},                       // just over the cap
		{"100000000000d", 0, true},                // overflow guard: day count too large
	}
	for _, tc := range cases {
		got, err := parseTTL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTTL(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTTL(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTTL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTokenStatus(t *testing.T) {
	past := "2000-01-01T00:00:00Z"
	future := "2999-01-01T00:00:00Z"
	revoked := "2026-06-01T00:00:00Z"
	cases := []struct {
		name string
		tok  cpclient.APIToken
		want string
	}{
		{"active-no-expiry", cpclient.APIToken{}, "active"},
		{"active-future-expiry", cpclient.APIToken{ExpiresAt: &future}, "active"},
		{"expired", cpclient.APIToken{ExpiresAt: &past}, "expired"},
		{"revoked-beats-expired", cpclient.APIToken{ExpiresAt: &past, RevokedAt: &revoked}, "revoked"},
	}
	for _, tc := range cases {
		if got := tokenStatus(tc.tok); got != tc.want {
			t.Errorf("tokenStatus(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
