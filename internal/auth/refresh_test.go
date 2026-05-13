// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"bytes"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

func TestGenerateRefreshTokenIsRandom(t *testing.T) {
	a, _, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	b, _, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	if a == b {
		t.Errorf("two refreshes are identical: %q", a)
	}
	if len(a) < 32 {
		t.Errorf("refresh plaintext = %q (len %d), want >= 32 chars (32 bytes base64-encoded)", a, len(a))
	}
}

func TestHashRefreshTokenDeterministic(t *testing.T) {
	plaintext, h1, err := auth.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	h2 := auth.HashRefreshToken(plaintext)
	if !bytes.Equal(h1, h2) {
		t.Errorf("hashes differ for same plaintext: %x vs %x", h1, h2)
	}
}

func TestHashRefreshTokenDistinguishes(t *testing.T) {
	h1 := auth.HashRefreshToken("token-one")
	h2 := auth.HashRefreshToken("token-two")
	if bytes.Equal(h1, h2) {
		t.Errorf("collisions on different inputs: %x", h1)
	}
}

func TestHashRefreshTokenLength(t *testing.T) {
	if got := auth.HashRefreshToken("anything"); len(got) != 32 {
		t.Errorf("hash len = %d, want 32 (sha256)", len(got))
	}
}
