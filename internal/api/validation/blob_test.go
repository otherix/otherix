// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import "testing"

func TestValidateBlobDigest(t *testing.T) {
	good := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := ValidateBlobDigest(good); err != nil {
		t.Errorf("ValidateBlobDigest(64 hex) = %v, want nil", err)
	}
	for _, bad := range []string{
		"",
		"abc",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",   // uppercase
		"../../../etc/otherix/secret-key-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",   // traversal, 64 chars
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",  // 65
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // non-hex letters
	} {
		if err := ValidateBlobDigest(bad); err == nil {
			t.Errorf("ValidateBlobDigest(%q) = nil, want error", bad)
		}
	}
}
