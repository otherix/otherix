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

func TestValidateImageURL(t *testing.T) {
	good := []string{
		"https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img",
		"https://example.test/img.qcow2",
		"https://example.test:8443/path/img.qcow2?x=1",
	}
	for _, in := range good {
		if err := validation.ValidateImageURL(in); err != nil {
			t.Errorf("ValidateImageURL(%q) = %v, want nil", in, err)
		}
	}
	bad := []struct {
		name string
		in   string
	}{
		{"http scheme", "http://example.test/img.qcow2"},
		{"http IMDS", "http://169.254.169.254/latest/meta-data/"},
		{"file scheme", "file:///etc/passwd"},
		{"ftp scheme", "ftp://example.test/img.qcow2"},
		{"gopher scheme", "gopher://example.test/img.qcow2"},
		{"empty", ""},
		{"relative path", "/images/img.qcow2"},
		{"scheme-relative", "//example.test/img.qcow2"},
		{"bare word", "img.qcow2"},
		{"https no host", "https:///img.qcow2"},
		{"unparseable", "https://exa mple.test/\x7f"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := validation.ValidateImageURL(tc.in); err == nil {
				t.Errorf("ValidateImageURL(%q) = nil, want error", tc.in)
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
