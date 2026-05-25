// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// Join-token wire format: "otx_join_" + base64url(32 random bytes).
// The double prefix (`otx_` family marker + `join_` subtype marker)
// separates these tokens from API tokens (`otx_<base64url>`) on first
// glance — useful when a CLI mishandling pastes the wrong token into
// the wrong slot. The family marker keeps a future single `otx_*`
// lookup table conceivable; the subtype keeps the two namespaces
// disjoint at the eyeball level.
const (
	joinTokenPrefix = "otx_join_"
	joinTokenBytes  = 32
)

// GenerateJoinToken returns a fresh join-token bundle: the plaintext to
// hand back to the operator exactly once on creation, and the storage
// hash for the join_tokens.token_hash column. There is no display
// prefix — join tokens are short-lived and never surface in any UI
// after the create response, so the disambiguation that api_tokens
// needs (prefix column) is irrelevant here.
func GenerateJoinToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, joinTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate join token: %v", err)
	}
	plaintext = joinTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), nil
}

// IsJoinTokenFormat reports whether s carries the join-token prefix.
// Step 2 redemption handler uses this to fail fast on tokens of the
// wrong subtype (e.g. an API token mistakenly submitted to the
// bootstrap endpoint).
func IsJoinTokenFormat(s string) bool {
	return strings.HasPrefix(s, joinTokenPrefix)
}
