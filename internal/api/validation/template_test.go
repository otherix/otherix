// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation_test

import (
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api/validation"
)

func TestValidateTemplateName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"happy", "ubuntu-noble-cloud", true},
		{"empty", "", false},
		{"leading whitespace", " name", false},
		{"trailing whitespace", "name ", false},
		{"too long", strings.Repeat("a", validation.TemplateNameMaxLength+1), false},
		{"max length", strings.Repeat("a", validation.TemplateNameMaxLength), true},
		{"unicode ok", "тестовый-шаблон", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validation.ValidateTemplateName(tc.in)
			if tc.ok && err != nil {
				t.Errorf("got %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("got nil, want error")
			}
		})
	}
}

func TestValidateOSFamily(t *testing.T) {
	for _, ok := range []string{"linux", "windows", "bsd", "other"} {
		if err := validation.ValidateOSFamily(ok); err != nil {
			t.Errorf("ValidateOSFamily(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "Linux", "macos", "win"} {
		if err := validation.ValidateOSFamily(bad); err == nil {
			t.Errorf("ValidateOSFamily(%q) = nil, want error", bad)
		}
	}
}

func TestValidateOSVariant(t *testing.T) {
	if err := validation.ValidateOSVariant(""); err != nil {
		t.Errorf("empty os_variant should be allowed, got %v", err)
	}
	if err := validation.ValidateOSVariant("ubuntu-2404"); err != nil {
		t.Errorf("happy path failed: %v", err)
	}
	if err := validation.ValidateOSVariant(strings.Repeat("a", validation.TemplateOSVariantMaxLength+1)); err == nil {
		t.Error("expected error for over-long variant, got nil")
	}
	if err := validation.ValidateOSVariant("ubuntu\x00"); err == nil {
		t.Error("expected error for control character, got nil")
	}
}

func TestValidateImageURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"https", "https://cloud.example.com/image.qcow2", true},
		{"http", "http://cloud.example.com/image.qcow2", true},
		{"empty", "", false},
		{"file scheme", "file:///srv/img.qcow2", false},
		{"ftp scheme", "ftp://example.com/img", false},
		{"no host", "https:///path", false},
		{"too long", "https://example.com/" + strings.Repeat("a", validation.TemplateImageURLMaxLength), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validation.ValidateImageURL(tc.in)
			if tc.ok && err != nil {
				t.Errorf("got %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("got nil, want error")
			}
		})
	}
}

func TestValidateImageChecksumSHA256(t *testing.T) {
	good := strings.Repeat("a", validation.TemplateImageChecksumLength)
	if err := validation.ValidateImageChecksumSHA256(good); err != nil {
		t.Errorf("got %v, want nil", err)
	}
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"too short", strings.Repeat("a", validation.TemplateImageChecksumLength-1)},
		{"too long", strings.Repeat("a", validation.TemplateImageChecksumLength+1)},
		{"uppercase", strings.Repeat("A", validation.TemplateImageChecksumLength)},
		{"non-hex", strings.Repeat("z", validation.TemplateImageChecksumLength)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validation.ValidateImageChecksumSHA256(tc.in); err == nil {
				t.Error("got nil, want error")
			}
		})
	}
}

func TestDecodeImageChecksumSHA256(t *testing.T) {
	hex := strings.Repeat("0a", 32) // 64 chars hex → 32 bytes
	b, err := validation.DecodeImageChecksumSHA256(hex)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(b) != 32 {
		t.Errorf("got %d bytes, want 32", len(b))
	}
	if _, err := validation.DecodeImageChecksumSHA256("nope"); err == nil {
		t.Error("expected error on invalid hex, got nil")
	}
}

func TestValidateImageFormat(t *testing.T) {
	for _, ok := range []string{"qcow2", "raw"} {
		if err := validation.ValidateImageFormat(ok); err != nil {
			t.Errorf("ValidateImageFormat(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "QCOW2", "vmdk"} {
		if err := validation.ValidateImageFormat(bad); err == nil {
			t.Errorf("ValidateImageFormat(%q) = nil, want error", bad)
		}
	}
}

func TestValidateImageSizeBytes(t *testing.T) {
	if err := validation.ValidateImageSizeBytes(1); err != nil {
		t.Errorf("got %v, want nil", err)
	}
	if err := validation.ValidateImageSizeBytes(0); err == nil {
		t.Error("expected error on zero, got nil")
	}
	if err := validation.ValidateImageSizeBytes(-1); err == nil {
		t.Error("expected error on negative, got nil")
	}
	if err := validation.ValidateImageSizeBytes(validation.TemplateMaxImageSizeBytes + 1); err == nil {
		t.Error("expected error past ceiling, got nil")
	}
}

func TestValidateVisibility(t *testing.T) {
	for _, ok := range []string{"public", "private"} {
		if err := validation.ValidateVisibility(ok); err != nil {
			t.Errorf("ValidateVisibility(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "Private", "internal"} {
		if err := validation.ValidateVisibility(bad); err == nil {
			t.Errorf("ValidateVisibility(%q) = nil, want error", bad)
		}
	}
}

func TestValidateTemplateSizingRanges(t *testing.T) {
	// CPU cores
	for _, n := range []int{validation.TemplateMinCPUCores, validation.TemplateMaxCPUCores} {
		if err := validation.ValidateTemplateCPUCores(n); err != nil {
			t.Errorf("CPU=%d: got %v, want nil", n, err)
		}
	}
	for _, n := range []int{0, validation.TemplateMaxCPUCores + 1} {
		if err := validation.ValidateTemplateCPUCores(n); err == nil {
			t.Errorf("CPU=%d: got nil, want error", n)
		}
	}
	// Memory
	for _, n := range []int{validation.TemplateMinMemoryMiB, validation.TemplateMaxMemoryMiB} {
		if err := validation.ValidateTemplateMemoryMiB(n); err != nil {
			t.Errorf("MEM=%d: got %v, want nil", n, err)
		}
	}
	for _, n := range []int{validation.TemplateMinMemoryMiB - 1, validation.TemplateMaxMemoryMiB + 1} {
		if err := validation.ValidateTemplateMemoryMiB(n); err == nil {
			t.Errorf("MEM=%d: got nil, want error", n)
		}
	}
	// Disk
	for _, n := range []int{validation.TemplateMinDiskGiB, validation.TemplateMaxDiskGiB} {
		if err := validation.ValidateTemplateDiskGiB(n); err != nil {
			t.Errorf("DISK=%d: got %v, want nil", n, err)
		}
	}
	for _, n := range []int{0, validation.TemplateMaxDiskGiB + 1} {
		if err := validation.ValidateTemplateDiskGiB(n); err == nil {
			t.Errorf("DISK=%d: got nil, want error", n)
		}
	}
}

func TestValidateTemplateMetadata(t *testing.T) {
	if err := validation.ValidateTemplateMetadata(nil); err != nil {
		t.Errorf("nil metadata should be allowed, got %v", err)
	}
	if err := validation.ValidateTemplateMetadata([]byte(`{"k":"v"}`)); err != nil {
		t.Errorf("small metadata should be allowed, got %v", err)
	}
	tooBig := make([]byte, validation.TemplateMaxMetadataBytes+1)
	if err := validation.ValidateTemplateMetadata(tooBig); err == nil {
		t.Error("expected error past ceiling, got nil")
	}
}
