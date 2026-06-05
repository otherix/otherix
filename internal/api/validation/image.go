// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"

	"github.com/otherix/otherix/internal/store"
)

// ImageChecksumHexLength is the rune length of a lowercase hex SHA-256
// digest. A VM's image_sha256 is stored as bytes; the API surface is hex.
const ImageChecksumHexLength = 64

// ValidateImageChecksumSHA256 returns nil when s is exactly 64 lowercase
// hex characters. The VM row's image_sha256 column is bytea, so the API
// edge converts hex <-> bytes via hex.DecodeString / hex.EncodeToString;
// this validator gates the wire-format string.
func ValidateImageChecksumSHA256(s string) error {
	if len(s) != ImageChecksumHexLength {
		return fmt.Errorf("image_sha256 must be exactly %d hex characters", ImageChecksumHexLength)
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			// ok
		default:
			return errors.New("image_sha256 must be lowercase hex (no uppercase, no separators)")
		}
	}
	return nil
}

// ValidateImageFormat returns nil when s is a recognised store.ImageFormat
// enum value (qcow2 or raw).
func ValidateImageFormat(s string) error {
	switch store.ImageFormat(s) {
	case store.ImageFormatQcow2, store.ImageFormatRaw:
		return nil
	}
	return fmt.Errorf("invalid image_format %q (must be one of: qcow2, raw)", s)
}
