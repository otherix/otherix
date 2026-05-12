// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/otherix/otherix/internal/store"
)

// Template-level bounds and constants. Each one mirrors a column
// CHECK or an OpenAPI `maxLength` / `maximum` entry; collected here
// so the API edge surfaces a clean 400 instead of a 23514
// check_violation surfacing from the storage layer.
const (
	// TemplateNameMaxLength matches `Template.name.maxLength` in the
	// OpenAPI spec. The SQL column is unbounded text; cap is API-edge.
	TemplateNameMaxLength = 255

	// TemplateOSVariantMaxLength caps the free-form variant string
	// (e.g. "ubuntu-2404", "rocky-9.4"). The SQL column is unbounded.
	TemplateOSVariantMaxLength = 255

	// TemplateImageURLMaxLength caps the source image URL. 2048 is the
	// HTTP URL practical ceiling and matches common form-input limits.
	TemplateImageURLMaxLength = 2048

	// TemplateImageChecksumLength is the rune length of a lowercase hex
	// SHA-256 digest. The SQL column is bytea; the API surface is hex.
	TemplateImageChecksumLength = 64

	// TemplateMaxImageSizeBytes is the API-edge ceiling on
	// image_size_bytes when the caller supplies it. 1 TiB; the column
	// is bigint without a CHECK, so this is a soft sanity cap.
	TemplateMaxImageSizeBytes int64 = 1 << 40

	// TemplateMaxMetadataBytes caps the marshalled JSONB metadata
	// payload. Large blobs in this column would bloat list responses
	// and the GIN index footprint.
	TemplateMaxMetadataBytes = 16 * 1024

	// Default/min/max sizing — match chk_templates_cpu /
	// chk_templates_memory / chk_templates_disk in 00001_init.sql and
	// the OpenAPI `Template.default_*` bounds.
	TemplateMinCPUCores    = 1
	TemplateMaxCPUCores    = 256
	TemplateMinMemoryMiB   = 128
	TemplateMaxMemoryMiB   = 4194304
	TemplateMinDiskGiB     = 1
	TemplateMaxDiskGiB     = 65536
	TemplateDefaultCPU     = 2
	TemplateDefaultMemMiB  = 2048
	TemplateDefaultDiskGiB = 20
)

// ValidateTemplateName returns nil when s is a syntactically valid
// template name: 1..255 runes, no leading or trailing whitespace.
// Global uniqueness (uq_templates_name) is enforced by the schema —
// this helper only catches malformed input.
func ValidateTemplateName(s string) error {
	if s == "" {
		return errors.New("name is required")
	}
	if s != strings.TrimSpace(s) {
		return errors.New("name must not have leading or trailing whitespace")
	}
	if utf8.RuneCountInString(s) > TemplateNameMaxLength {
		return fmt.Errorf("name is too long (max %d runes)", TemplateNameMaxLength)
	}
	return nil
}

// ValidateOSFamily returns nil when s is a recognised store.OsFamily
// enum value. Anchored to the SQL enum os_family.
func ValidateOSFamily(s string) error {
	switch store.OsFamily(s) {
	case store.OsFamilyLinux, store.OsFamilyWindows, store.OsFamilyBsd, store.OsFamilyOther:
		return nil
	}
	return fmt.Errorf("invalid os_family %q (must be one of: linux, windows, bsd, other)", s)
}

// ValidateOSVariant returns nil when s is acceptable as os_variant.
// The empty string is allowed (the column has DEFAULT ”). Non-empty
// strings must fit the rune cap and contain no control characters.
func ValidateOSVariant(s string) error {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > TemplateOSVariantMaxLength {
		return fmt.Errorf("os_variant is too long (max %d runes)", TemplateOSVariantMaxLength)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("os_variant must not contain control characters")
		}
	}
	return nil
}

// ValidateImageURL returns nil when s is a syntactically valid image
// source URL: parseable, http or https scheme, host present, length
// ≤ 2048. The fetch itself happens agent-side; this is an edge sanity
// check, not a reachability probe.
func ValidateImageURL(s string) error {
	if s == "" {
		return errors.New("image_url is required")
	}
	if len(s) > TemplateImageURLMaxLength {
		return fmt.Errorf("image_url is too long (max %d characters)", TemplateImageURLMaxLength)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid image_url: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("image_url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("image_url must include a host")
	}
	return nil
}

// ValidateImageChecksumSHA256 returns nil when s is exactly 64
// lowercase hex characters. The storage column is bytea, so the API
// edge converts hex ↔ bytes via hex.DecodeString / hex.EncodeToString;
// this validator gates the wire-format string.
func ValidateImageChecksumSHA256(s string) error {
	if len(s) != TemplateImageChecksumLength {
		return fmt.Errorf("image_checksum_sha256 must be exactly %d hex characters", TemplateImageChecksumLength)
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			// ok
		default:
			return errors.New("image_checksum_sha256 must be lowercase hex (no uppercase, no separators)")
		}
	}
	return nil
}

// DecodeImageChecksumSHA256 returns the 32-byte digest for s after
// running ValidateImageChecksumSHA256. The validator already proves
// the hex.DecodeString call cannot fail, but the error path is
// preserved for defence-in-depth.
func DecodeImageChecksumSHA256(s string) ([]byte, error) {
	if err := ValidateImageChecksumSHA256(s); err != nil {
		return nil, err
	}
	return hex.DecodeString(s)
}

// ValidateImageFormat returns nil when s is a recognised
// store.ImageFormat enum value. Anchored to the SQL enum image_format.
func ValidateImageFormat(s string) error {
	switch store.ImageFormat(s) {
	case store.ImageFormatQcow2, store.ImageFormatRaw:
		return nil
	}
	return fmt.Errorf("invalid image_format %q (must be one of: qcow2, raw)", s)
}

// ValidateImageSizeBytes returns nil when n is positive and within
// the 1 TiB sanity ceiling. Callers gate the validator on "value
// supplied" — image_size_bytes is nullable.
func ValidateImageSizeBytes(n int64) error {
	if n <= 0 {
		return errors.New("image_size_bytes must be positive when set")
	}
	if n > TemplateMaxImageSizeBytes {
		return fmt.Errorf("image_size_bytes exceeds the %d-byte API ceiling", TemplateMaxImageSizeBytes)
	}
	return nil
}

// ValidateVisibility returns nil when s is "public" or "private".
// Used by the setVisibility handler; the create / clone paths fix
// visibility to "private" without consulting this validator.
func ValidateVisibility(s string) error {
	switch s {
	case "public", "private":
		return nil
	}
	return fmt.Errorf("invalid visibility %q (must be one of: public, private)", s)
}

// ValidateTemplateCPUCores returns nil when n is within the column's
// CHECK range (1..256).
func ValidateTemplateCPUCores(n int) error {
	if n < TemplateMinCPUCores || n > TemplateMaxCPUCores {
		return fmt.Errorf("default_cpu_cores must be between %d and %d", TemplateMinCPUCores, TemplateMaxCPUCores)
	}
	return nil
}

// ValidateTemplateMemoryMiB returns nil when n is within the column's
// CHECK range (128..4194304).
func ValidateTemplateMemoryMiB(n int) error {
	if n < TemplateMinMemoryMiB || n > TemplateMaxMemoryMiB {
		return fmt.Errorf("default_memory_mib must be between %d and %d", TemplateMinMemoryMiB, TemplateMaxMemoryMiB)
	}
	return nil
}

// ValidateTemplateDiskGiB returns nil when n is within the column's
// CHECK range (1..65536).
func ValidateTemplateDiskGiB(n int) error {
	if n < TemplateMinDiskGiB || n > TemplateMaxDiskGiB {
		return fmt.Errorf("default_disk_gib must be between %d and %d", TemplateMinDiskGiB, TemplateMaxDiskGiB)
	}
	return nil
}

// ValidateTemplateMetadata returns nil when raw is empty (so the
// caller can substitute the JSONB default '{}') or a JSON object
// payload that fits the size cap. The contents are not validated
// further — metadata is a free-form labels bag.
func ValidateTemplateMetadata(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > TemplateMaxMetadataBytes {
		return fmt.Errorf("metadata exceeds the %d-byte ceiling", TemplateMaxMetadataBytes)
	}
	return nil
}
