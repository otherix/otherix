// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
)

func testSecret() []byte {
	return []byte("test-secret-32-bytes-padding-pad-")
}

func TestIssueAndParseRoundTrip(t *testing.T) {
	uid := uuid.New()
	tok, err := auth.IssueAccessToken(testSecret(), uid, "operator", time.Hour)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if tok == "" {
		t.Fatal("IssueAccessToken returned empty string")
	}

	claims, err := auth.ParseAccessToken(testSecret(), tok)
	if err != nil {
		t.Fatalf("ParseAccessToken: %v", err)
	}
	if claims.Subject != uid.String() {
		t.Errorf("Subject = %q, want %q", claims.Subject, uid)
	}
	if claims.Role != "operator" {
		t.Errorf("Role = %q, want %q", claims.Role, "operator")
	}
	if claims.ID == "" {
		t.Errorf("jti is empty, want random UUID")
	}
	if claims.Issuer != "otherix" {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, "otherix")
	}
}

func TestParseAccessTokenExpired(t *testing.T) {
	uid := uuid.New()
	tok, err := auth.IssueAccessToken(testSecret(), uid, "viewer", -time.Second)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	_, err = auth.ParseAccessToken(testSecret(), tok)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

func TestParseAccessTokenWrongSecret(t *testing.T) {
	tok, err := auth.IssueAccessToken(testSecret(), uuid.New(), "viewer", time.Hour)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	_, err = auth.ParseAccessToken([]byte("different-secret-padding-padding"), tok)
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestParseAccessTokenTampered(t *testing.T) {
	tok, err := auth.IssueAccessToken(testSecret(), uuid.New(), "viewer", time.Hour)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	// Flip the first byte of the signature segment to a guaranteed-different value.
	// Avoid the last byte: under non-strict base64 decoding the trailing padding
	// bits make some single-char swaps decode to the same signature bytes, so the
	// HMAC still verifies. The first byte has no such ambiguity.
	sigStart := strings.LastIndex(tok, ".") + 1
	replacement := byte('X')
	if tok[sigStart] == replacement {
		replacement = 'Y'
	}
	tampered := tok[:sigStart] + string(replacement) + tok[sigStart+1:]
	if _, err := auth.ParseAccessToken(testSecret(), tampered); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestParseAccessTokenGarbage(t *testing.T) {
	cases := []string{"", "not.a.jwt", "abc.def", strings.Repeat("a", 1000)}
	for _, raw := range cases {
		if _, err := auth.ParseAccessToken(testSecret(), raw); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("ParseAccessToken(%q) err = %v, want ErrInvalidToken", raw, err)
		}
	}
}
