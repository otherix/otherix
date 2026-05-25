// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"bytes"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// TestHashToken_DeterministicAndDistinct asserts the post-refactor
// invariants: same plaintext hashes to the same bytes (lookup works),
// distinct plaintexts produce distinct hashes (collision-free for
// random tokens), and the digest length matches sha256 (32 bytes —
// the column type encoded as bytea).
func TestHashToken_DeterministicAndDistinct(t *testing.T) {
	const a = "otx_join_aaaaaa"
	const b = "otx_join_bbbbbb"

	if got := auth.HashToken(a); len(got) != 32 {
		t.Fatalf("digest length = %d, want 32", len(got))
	}
	if got, want := auth.HashToken(a), auth.HashToken(a); !bytes.Equal(got, want) {
		t.Errorf("HashToken(%q) non-deterministic: %x vs %x", a, got, want)
	}
	if bytes.Equal(auth.HashToken(a), auth.HashToken(b)) {
		t.Errorf("HashToken(%q) == HashToken(%q), digests must differ", a, b)
	}
}

// TestHashAPIToken_HashTokenAlias asserts the legacy HashAPIToken
// wrapper produces identical output to HashToken — guarantees the
// post-refactor contract for existing apitokens call sites.
func TestHashAPIToken_HashTokenAlias(t *testing.T) {
	const plain = "otx_abcdef"
	if !bytes.Equal(auth.HashAPIToken(plain), auth.HashToken(plain)) {
		t.Error("HashAPIToken not equal HashToken — refactor regression")
	}
}

// TestHashRefreshToken_HashTokenAlias asserts the legacy
// HashRefreshToken wrapper produces identical output to HashToken.
func TestHashRefreshToken_HashTokenAlias(t *testing.T) {
	const plain = "deadbeefdeadbeefdeadbeef"
	if !bytes.Equal(auth.HashRefreshToken(plain), auth.HashToken(plain)) {
		t.Error("HashRefreshToken not equal HashToken — refactor regression")
	}
}
