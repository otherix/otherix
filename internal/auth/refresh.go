// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// refreshTokenBytes is the entropy in a refresh token plaintext. 32 bytes
// (256 bits) gives 43 base64url characters in the wire form, which is
// compact and uniformly indistinguishable from random.
const refreshTokenBytes = 32

// GenerateRefreshToken returns a fresh (plaintext, hash) pair. Plaintext
// is what the client receives; hash is what the server stores. Plaintext
// is base64url-encoded random bytes — opaque, not a JWT.
func GenerateRefreshToken() (plaintext string, hash []byte, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %v", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), nil
}

// HashRefreshToken returns the storage hash of plaintext. Used by Service
// for lookups: verification is "do the stored hashes match", not a
// password-style verifier (no per-token salt — the token itself carries
// 256 bits of entropy, so a salt buys nothing and would defeat lookup).
//
// Deprecated alias; prefer auth.HashToken directly. Retained so
// existing call sites compile after the consolidation в tokens.go.
func HashRefreshToken(plaintext string) []byte {
	return HashToken(plaintext)
}
