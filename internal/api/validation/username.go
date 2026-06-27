// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// UsernameMinLength / UsernameMaxLength bound the username and match
// `User.username` minLength/maxLength in api/openapi/control-plane.yaml.
const (
	UsernameMinLength = 3
	UsernameMaxLength = 32
)

// usernamePattern is a DNS-1123-label-style handle: lowercase ASCII
// alphanumerics plus interior hyphens (first and last rune must be
// alphanumeric). Uppercase is rejected, not folded, so the stored value
// is the submitted value and login/uniqueness are exact-match.
var usernamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateUsername reports whether s is a valid username: 3..32 runes,
// matching usernamePattern.
func ValidateUsername(s string) error {
	if s == "" {
		return fmt.Errorf("username is required")
	}
	n := utf8.RuneCountInString(s)
	if n < UsernameMinLength || n > UsernameMaxLength {
		return fmt.Errorf("username must be %d-%d characters", UsernameMinLength, UsernameMaxLength)
	}
	if !usernamePattern.MatchString(s) {
		return fmt.Errorf("username must be lowercase letters, digits, and interior hyphens (e.g. web-admin)")
	}
	return nil
}
