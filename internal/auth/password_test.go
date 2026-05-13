// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

func TestHashPasswordEmptyRejected(t *testing.T) {
	if _, err := auth.HashPassword(""); err == nil {
		t.Errorf("HashPassword(\"\") = nil, want error")
	}
}

func TestHashPasswordPHCFormat(t *testing.T) {
	hash, err := auth.HashPassword("hunter2-correct-horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash = %q, want PHC argon2id prefix", hash)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Errorf("hash sections = %d, want 6 (PHC format)", len(parts))
	}
}

func TestHashPasswordIsRandomized(t *testing.T) {
	a, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword 1: %v", err)
	}
	b, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword 2: %v", err)
	}
	if a == b {
		t.Errorf("two hashes of same plaintext are identical; salt is not random")
	}
}

func TestVerifyPasswordRoundTrip(t *testing.T) {
	const pw = "correct-horse-battery-staple"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := auth.VerifyPassword(hash, pw)
	if err != nil {
		t.Fatalf("VerifyPassword(correct): err = %v, want nil", err)
	}
	if !ok {
		t.Errorf("VerifyPassword(correct) = false, want true")
	}

	ok, err = auth.VerifyPassword(hash, "wrong")
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): err = %v, want nil (mismatch is not an error)", err)
	}
	if ok {
		t.Errorf("VerifyPassword(wrong) = true, want false")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	tests := []struct {
		name   string
		stored string
	}{
		{name: "empty", stored: ""},
		{name: "wrong sections", stored: "$argon2id$v=19$abc"},
		{name: "wrong algo", stored: "$argon2i$v=19$m=4096,t=2,p=1$c2FsdA$aGFzaA"},
		{name: "garbage", stored: "not even close"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := auth.VerifyPassword(tc.stored, "anything")
			if err == nil {
				t.Errorf("VerifyPassword(%q) err = nil, want error", tc.stored)
			}
			if ok {
				t.Errorf("VerifyPassword(%q) = true, want false on malformed input", tc.stored)
			}
		})
	}
}
