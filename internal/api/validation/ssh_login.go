// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"fmt"
	"regexp"
)

// sshLoginPattern is the valid SSH principal charset: a lowercase start
// ([a-z_]) followed by up to 31 [a-z0-9_-] characters (32-char cap, the
// conventional Linux login limit). The guest sshd is the sole authority for
// whether it accepts the login; this pattern only guarantees the value is a
// safe single principal (no shell metacharacters, path separators, or
// whitespace) before it is baked into a certificate or printed in an
// ssh <login>@host command.
var sshLoginPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidateSSHLogin reports whether login is a valid SSH principal and returns
// it unchanged when so. It rejects empty, over-long, and any value carrying
// characters outside sshLoginPattern.
func ValidateSSHLogin(login string) (string, error) {
	if !sshLoginPattern.MatchString(login) {
		return "", fmt.Errorf("login must match [a-z_][a-z0-9_-]* (max 32 chars)")
	}
	return login, nil
}
