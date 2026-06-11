// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"strings"
	"testing"
)

// argonParams extracts the cost-parameter segment ("m=...,t=...,p=...") from
// an argon2id PHC string: $argon2id$v=19$m=..,t=..,p=..$salt$hash. Fails the
// test if the string does not have the expected five-segment shape.
func argonParams(t *testing.T, phc string) string {
	t.Helper()
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		t.Fatalf("argonParams(%q): not a well-formed argon2id PHC string", phc)
	}
	return parts[3]
}

// TestDummyLoginHashParamsMatchActiveArgonParams guards the M6 timing fix
// against init-order regressions: the dummy hash must carry the same argon2
// cost parameters as a hash produced by the active HashPassword
// configuration. A package-level var initializer runs before the
// test_fast_argon init() override and would capture the production cost,
// inverting the timing equalization under test builds; lazy initialization
// keeps the two in lockstep in every build.
func TestDummyLoginHashParamsMatchActiveArgonParams(t *testing.T) {
	fresh, err := HashPassword("x")
	if err != nil {
		t.Fatalf("HashPassword(\"x\") error = %v, want nil", err)
	}
	got := argonParams(t, dummyLoginHash())
	want := argonParams(t, fresh)
	if got != want {
		t.Errorf("dummyLoginHash params = %q, want %q (active HashPassword params)", got, want)
	}
}

// TestDummyLoginHashIsRealArgon2id guards the user-enumeration timing fix
// (audit M6): the dummy hash verified on Login's user-not-found path must
// be a well-formed argon2id PHC string so VerifyPassword runs the full KDF
// rather than bailing out on a parse error. A (false, nil) result proves
// the mismatch path was reached, i.e. the KDF cost was actually paid.
func TestDummyLoginHashIsRealArgon2id(t *testing.T) {
	ok, err := VerifyPassword(dummyLoginHash(), "anything")
	if err != nil {
		t.Fatalf("VerifyPassword(dummyLoginHash(), ...) error = %v, want nil (hash must parse)", err)
	}
	if ok {
		t.Fatal("VerifyPassword(dummyLoginHash(), \"anything\") = true, want false")
	}
}
