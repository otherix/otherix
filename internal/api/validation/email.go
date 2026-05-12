// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package validation hosts request-payload validators shared between
// HTTP handlers and the bootstrap path. Helpers return plain Go errors;
// callers are responsible for mapping to the API error envelope.
package validation

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// EmailMaxLength bounds the stored email length and matches
// `User.email.maxLength` in api/openapi/control-plane.yaml.
const EmailMaxLength = 255

// ValidateEmail reports whether s is a syntactically-valid email
// address per RFC 5322 (via net/mail) and within the length bound. The
// helper rejects display-name forms ("Name <a@b>") because the rest of
// the system stores the bare address only.
func ValidateEmail(s string) error {
	if s == "" {
		return errors.New("email is required")
	}
	if utf8.RuneCountInString(s) > EmailMaxLength {
		return fmt.Errorf("email must not exceed %d characters", EmailMaxLength)
	}

	addr, err := mail.ParseAddress(s)
	if err != nil {
		return fmt.Errorf("invalid email format: %v", err)
	}
	if addr.Address != strings.TrimSpace(s) {
		return errors.New("email must be a bare address (no display name)")
	}
	return nil
}
