// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import "net/netip"

// sourceIPAllowed reports whether client is permitted by a published listener's
// optional source-IP allowlist. An empty (or nil) cidrs list means no
// restriction (open to any source that reaches the port). A non-empty list
// admits client only when it falls inside at least one parsed prefix.
//
// The match is fail-closed: an invalid client denies, and a cidrs entry that
// fails to parse is skipped so a malformed allowlist entry can never widen
// access. Modeled on auth.SourceIPAllows, extended from a single pin to a list.
func sourceIPAllowed(cidrs []string, client netip.Addr) bool {
	if len(cidrs) == 0 {
		return true
	}
	if !client.IsValid() {
		return false
	}
	for _, c := range cidrs {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			continue
		}
		if prefix.Contains(client) {
			return true
		}
	}
	return false
}
