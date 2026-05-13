// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

func TestGenerateAPITokenShape(t *testing.T) {
	plaintext, hash, prefix, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	if !strings.HasPrefix(plaintext, "otx_") {
		t.Errorf("plaintext = %q, want otx_ prefix", plaintext)
	}
	if !auth.IsAPITokenFormat(plaintext) {
		t.Errorf("IsAPITokenFormat(%q) = false, want true", plaintext)
	}
	if len(hash) != 32 {
		t.Errorf("hash len = %d, want 32 (sha256)", len(hash))
	}
	if len(prefix) != 8 {
		t.Errorf("prefix len = %d, want 8", len(prefix))
	}
	if !strings.HasPrefix(plaintext, prefix) {
		t.Errorf("prefix %q is not a prefix of plaintext %q", prefix, plaintext)
	}
}

func TestGenerateAPITokenIsRandom(t *testing.T) {
	a, _, _, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	b, _, _, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	if a == b {
		t.Errorf("two api tokens are identical: %q", a)
	}
}

func TestHashAPITokenDeterministic(t *testing.T) {
	plaintext, h1, _, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	h2 := auth.HashAPIToken(plaintext)
	if !bytes.Equal(h1, h2) {
		t.Errorf("hashes differ for same plaintext")
	}
}

func TestIsAPITokenFormat(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "otx_abc", want: true},
		{in: "otx_", want: true},
		{in: "OTX_abc", want: false},
		{in: "abc", want: false},
		{in: "", want: false},
		{in: "Bearer otx_abc", want: false},
	}
	for _, tc := range tests {
		if got := auth.IsAPITokenFormat(tc.in); got != tc.want {
			t.Errorf("IsAPITokenFormat(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
