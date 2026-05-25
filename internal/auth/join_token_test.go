// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// TestGenerateJoinToken_FormatAndUniqueness verifies:
//   - plaintext carries the otx_join_ prefix;
//   - distinct calls return distinct plaintexts (crypto/rand variance);
//   - hash matches sha256(plaintext) and is deterministic per plaintext;
//   - hash length is 32 bytes (the bytea column shape).
func TestGenerateJoinToken_FormatAndUniqueness(t *testing.T) {
	plain1, hash1, err := auth.GenerateJoinToken()
	if err != nil {
		t.Fatalf("GenerateJoinToken #1: %v", err)
	}
	plain2, hash2, err := auth.GenerateJoinToken()
	if err != nil {
		t.Fatalf("GenerateJoinToken #2: %v", err)
	}

	if !strings.HasPrefix(plain1, "otx_join_") {
		t.Errorf("plaintext %q missing otx_join_ prefix", plain1)
	}
	if plain1 == plain2 {
		t.Error("crypto/rand collision: two GenerateJoinToken calls returned equal plaintexts")
	}
	if len(hash1) != 32 {
		t.Errorf("hash1 length = %d, want 32", len(hash1))
	}
	if !bytes.Equal(hash1, auth.HashToken(plain1)) {
		t.Error("hash1 != HashToken(plain1) — generation diverged from storage")
	}
	if bytes.Equal(hash1, hash2) {
		t.Error("distinct plaintexts produced equal hashes")
	}
}

// TestIsJoinTokenFormat_PrefixDetection verifies the dispatch helper
// used by the future redemption handler accepts join tokens and
// rejects API tokens / random strings.
func TestIsJoinTokenFormat_PrefixDetection(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"otx_join_abcdef", true},
		{"otx_join_", true}, // technically allowed by prefix check; format validation happens elsewhere
		{"otx_abcdef", false},
		{"otx_", false},
		{"foo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := auth.IsJoinTokenFormat(c.in); got != c.want {
			t.Errorf("IsJoinTokenFormat(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
