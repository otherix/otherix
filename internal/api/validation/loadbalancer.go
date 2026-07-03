// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"fmt"
	"net/netip"
)

// DefaultLBProtocol is the protocol a published load balancer takes when
// none is supplied. It is the only value v1 accepts.
const DefaultLBProtocol = "tcp"

// ValidateLBProtocol accepts only "tcp". The field exists so UDP is a later
// additive value rather than a schema break.
func ValidateLBProtocol(p string) error {
	if p != "tcp" {
		return fmt.Errorf("invalid protocol %q (must be one of: tcp)", p)
	}
	return nil
}

// ValidateSourceCIDR accepts any CIDR netip.ParsePrefix parses, including a
// single host (/32) and the whole internet (/0), IPv4 or IPv6. A bare IP with
// no prefix is rejected - source allowlist entries must be explicit CIDRs.
func ValidateSourceCIDR(s string) error {
	if _, err := netip.ParsePrefix(s); err != nil {
		return fmt.Errorf("invalid source cidr %q: %v", s, err)
	}
	return nil
}

// MaxSourceCIDRs bounds a published listener's client-IP allowlist so the row
// JSON and the pushed declared state stay small.
const MaxSourceCIDRs = 64

// ValidateSourceCIDRs rejects a list longer than MaxSourceCIDRs, then validates
// each entry via ValidateSourceCIDR.
func ValidateSourceCIDRs(cidrs []string) error {
	if len(cidrs) > MaxSourceCIDRs {
		return fmt.Errorf("too many source cidrs: %d (max %d)", len(cidrs), MaxSourceCIDRs)
	}
	for _, c := range cidrs {
		if err := ValidateSourceCIDR(c); err != nil {
			return err
		}
	}
	return nil
}
