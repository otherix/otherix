// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// API-token wire format: "otx_" + base64url(32 random bytes). The prefix
// lets the authn middleware distinguish API tokens from JWTs in a single
// header (`Authorization: Bearer ...`) without a probe.
const (
	apiTokenPrefix = "otx_"
	apiTokenBytes  = 32
	// apiTokenDisplayPrefixLen is the number of leading characters of
	// the plaintext token stored in api_tokens.prefix and surfaced in
	// list responses for visual identification. 8 chars covers "otx_"
	// + 4 random characters, enough to disambiguate without leaking
	// useful entropy to an attacker.
	apiTokenDisplayPrefixLen = 8
)

// GenerateAPIToken returns a fresh API-token bundle: the plaintext to
// hand to the user once at creation, the storage hash, and the display
// prefix (first 8 chars of plaintext) for the api_tokens.prefix column.
func GenerateAPIToken() (plaintext string, hash []byte, displayPrefix string, err error) {
	buf := make([]byte, apiTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("generate api token: %v", err)
	}
	plaintext = apiTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), plaintext[:apiTokenDisplayPrefixLen], nil
}

// HashAPIToken returns the storage hash of plaintext. The middleware
// hashes whatever it lifted from the Authorization header and looks
// it up in api_tokens.token_hash; equal hashes mean equal plaintexts.
//
// Deprecated alias; prefer auth.HashToken directly. Retained so
// existing call sites compile after the consolidation в tokens.go.
func HashAPIToken(plaintext string) []byte {
	return HashToken(plaintext)
}

// IsAPITokenFormat reports whether s looks like an API token (i.e.
// carries the otx_ prefix). The middleware uses this to dispatch
// between JWT verification and API-token lookup. It does not validate
// the token — only its shape.
func IsAPITokenFormat(s string) bool {
	return strings.HasPrefix(s, apiTokenPrefix)
}
