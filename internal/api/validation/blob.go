// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import "errors"

// ErrInvalidBlobDigest is returned for a content address that is not a 64-char
// lowercase hex sha256.
var ErrInvalidBlobDigest = errors.New("blob digest must be a 64-char lowercase hex sha256")

// ValidateBlobDigest reports whether d is a 64-char lowercase hex string. A blob
// digest reported by an agent becomes a holder-discovery key and flows back out
// as a path component on the holder; a malformed value is rejected so it can
// never be a bad key or a path-injection sink.
func ValidateBlobDigest(d string) error {
	if len(d) != 64 {
		return ErrInvalidBlobDigest
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrInvalidBlobDigest
		}
	}
	return nil
}
