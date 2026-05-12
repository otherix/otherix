// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Password length bounds. Mirrors `password.minLength` /
// `password.maxLength` in api/openapi/control-plane.yaml. The chosen
// minimum follows NIST SP 800-63B guidance: long passphrases without
// composition rules ("must include a digit, must include …") are
// stronger and more usable than short complex passwords.
const (
	PasswordMinLength = 12
	PasswordMaxLength = 256
)

// ValidatePassword reports whether s satisfies the length policy. No
// composition rules are enforced.
func ValidatePassword(s string) error {
	if s == "" {
		return errors.New("password is required")
	}
	n := utf8.RuneCountInString(s)
	if n < PasswordMinLength {
		return fmt.Errorf("password must be at least %d characters", PasswordMinLength)
	}
	if n > PasswordMaxLength {
		return fmt.Errorf("password must not exceed %d characters", PasswordMaxLength)
	}
	return nil
}
