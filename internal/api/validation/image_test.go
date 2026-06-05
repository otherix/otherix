// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api/validation"
)

func TestValidateImageChecksumSHA256(t *testing.T) {
	good := strings.Repeat("a", validation.ImageChecksumHexLength)
	if err := validation.ValidateImageChecksumSHA256(good); err != nil {
		t.Errorf("ValidateImageChecksumSHA256(valid) = %v, want nil", err)
	}
	bad := []struct {
		name string
		in   string
	}{
		{"too short", strings.Repeat("a", validation.ImageChecksumHexLength-1)},
		{"too long", strings.Repeat("a", validation.ImageChecksumHexLength+1)},
		{"uppercase", strings.Repeat("A", validation.ImageChecksumHexLength)},
		{"non-hex", strings.Repeat("z", validation.ImageChecksumHexLength)},
		{"separator", strings.Repeat("a", validation.ImageChecksumHexLength-1) + ":"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := validation.ValidateImageChecksumSHA256(tc.in); err == nil {
				t.Errorf("ValidateImageChecksumSHA256(%q) = nil, want error", tc.in)
			}
		})
	}
}

func TestValidateImageFormat(t *testing.T) {
	for _, ok := range []string{"qcow2", "raw"} {
		if err := validation.ValidateImageFormat(ok); err != nil {
			t.Errorf("ValidateImageFormat(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "vmdk", "QCOW2", "iso"} {
		if err := validation.ValidateImageFormat(bad); err == nil {
			t.Errorf("ValidateImageFormat(%q) = nil, want error", bad)
		}
	}
}
