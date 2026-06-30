// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DNSLabelMaxLength bounds an RFC 1123 DNS label and matches the
// `maxLength` on `NodeCreate.name` and `VMCreate.name` in
// api/openapi/control-plane.yaml.
const DNSLabelMaxLength = 63

// dnsLabelRe matches a lowercase RFC 1123 DNS label: alphanumerics with
// internal hyphens, starting AND ending alphanumeric. The optional
// group lets a single character like "a" match.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateDNSLabel returns nil when s is a valid lowercase RFC 1123 DNS
// label: 1..63 runes of [a-z0-9] and '-', starting and ending
// alphanumeric. Lowercase-only is deliberate: the name becomes a guest
// hostname and a cert SAN component, both case-insensitive domains, and
// rejecting uppercase up front avoids "Demo" vs "demo" hostname / SAN
// collisions.
func ValidateDNSLabel(s string) error {
	if s == "" {
		return errors.New("name is required")
	}
	if utf8.RuneCountInString(s) > DNSLabelMaxLength {
		return fmt.Errorf("name is too long (max %d characters)", DNSLabelMaxLength)
	}
	if !dnsLabelRe.MatchString(s) {
		return fmt.Errorf("invalid name %q (must be a lowercase RFC 1123 DNS label: [a-z0-9] and '-', start and end alphanumeric, max %d)", s, DNSLabelMaxLength)
	}
	return nil
}

// DNSDomainMaxLength bounds an RFC 1035 DNS domain name (the textual
// representation, excluding the trailing dot).
const DNSDomainMaxLength = 253

// ValidateDNSDomain returns nil when s is a valid lowercase DNS domain
// name: one or more RFC 1123 DNS labels joined by '.', each label
// 1..63 runes of [a-z0-9] and '-' starting and ending alphanumeric,
// the whole name at most 253 runes and carrying no leading, trailing,
// or empty label. Lowercase-only mirrors ValidateDNSLabel: the value
// becomes a host-suffix component in case-insensitive DNS and cert
// SANs, so uppercase is rejected up front to avoid collisions.
func ValidateDNSDomain(s string) error {
	if s == "" {
		return errors.New("domain is required")
	}
	if utf8.RuneCountInString(s) > DNSDomainMaxLength {
		return fmt.Errorf("domain is too long (max %d characters)", DNSDomainMaxLength)
	}
	for _, label := range strings.Split(s, ".") {
		if !dnsLabelRe.MatchString(label) || utf8.RuneCountInString(label) > DNSLabelMaxLength {
			return fmt.Errorf("invalid domain %q (each label must be a lowercase RFC 1123 DNS label: [a-z0-9] and '-', start and end alphanumeric, max %d)", s, DNSLabelMaxLength)
		}
	}
	return nil
}

// ValidateVMName returns nil when s is a valid VM name. The name flows
// into the cidata meta-data (local-hostname / instance-id) and the
// console-stream URL path, so it is constrained to a lowercase RFC 1123
// DNS label.
func ValidateVMName(s string) error { return ValidateDNSLabel(s) }

// ValidateNodeName returns nil when s is a valid node name. The name
// flows into the issued agent cert CN `node-<name>` and the SAN
// `node-<name>.agents.otherix.local`, so it is constrained to a
// lowercase RFC 1123 DNS label.
func ValidateNodeName(s string) error { return ValidateDNSLabel(s) }
