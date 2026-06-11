// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import "testing"

// TestDummyLoginHashIsRealArgon2id guards the user-enumeration timing fix
// (audit M6): the dummy hash verified on Login's user-not-found path must
// be a well-formed argon2id PHC string so VerifyPassword runs the full KDF
// rather than bailing out on a parse error. A (false, nil) result proves
// the mismatch path was reached, i.e. the KDF cost was actually paid.
func TestDummyLoginHashIsRealArgon2id(t *testing.T) {
	ok, err := VerifyPassword(dummyLoginHash, "anything")
	if err != nil {
		t.Fatalf("VerifyPassword(dummyLoginHash, ...) error = %v, want nil (hash must parse)", err)
	}
	if ok {
		t.Fatal("VerifyPassword(dummyLoginHash, \"anything\") = true, want false")
	}
}
