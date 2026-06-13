// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import "regexp"

// rfc1123Label matches a lowercase RFC1123 DNS label: 1..63 chars, [a-z0-9-],
// first and last alphanumeric.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validVMName reports whether name is a lowercase RFC1123 DNS label (1..63
// chars, [a-z0-9-], first and last alphanumeric). The agent mirrors the
// control-plane VM-name constraint as defense-in-depth: the name is formatted
// into cloud-init YAML, so a name with a newline or YAML metacharacter must
// never reach the builder even if a future CP path stops validating (audit
// R2-M5).
func validVMName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	return rfc1123Label.MatchString(name)
}
