// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import "net/netip"

// SourceIPAllows reports whether client satisfies a grant's optional source-IP
// pin. A nil pin means no restriction (from anywhere). A set pin is a CIDR
// ("203.0.113.0/24") or a bare address ("198.51.100.7"); the client must fall
// within the CIDR or equal the bare address. A pin that fails to parse denies
// (fail-closed) - create-time validation makes an unparseable stored pin
// unreachable, but the runtime check never fails open.
func SourceIPAllows(pin *string, client netip.Addr) bool {
	if pin == nil {
		return true
	}
	if prefix, err := netip.ParsePrefix(*pin); err == nil {
		return prefix.Contains(client)
	}
	if addr, err := netip.ParseAddr(*pin); err == nil {
		return addr == client
	}
	return false
}
