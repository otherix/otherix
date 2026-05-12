// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/otherix/otherix/internal/store"
)

// NetworkNameMaxLength bounds networks.name and matches
// `Network.name.maxLength` in api/openapi/control-plane.yaml. The
// schema column is unbounded text; the cap is API-edge only.
const NetworkNameMaxLength = 255

// MTU bounds. DefaultMTU is the API-default value used when the
// caller does not specify one on Create — it matches the column
// default introduced in migration 00004. MinMTU is the IPv4 minimum
// per RFC 791 §3.2 (also the OpenAPI lower bound). MaxMTU is the
// practical jumbo-frame ceiling; values above that fail on most
// switches and bridges.
const (
	DefaultMTU = 1500
	MinMTU     = 68
	MaxMTU     = 9216
)

// VLAN tag bounds. The schema CHECK enforces the same range; this
// constant pair lets the API edge return a clean 400 instead of
// surfacing a 23514 (check_violation) at the storage layer.
const (
	MinVLANTag = 1
	MaxVLANTag = 4094
)

// linuxBridgeNameRe matches a syntactically valid Linux bridge
// interface name: 1..15 ASCII characters, leading letter, body in
// [A-Za-z0-9_-]. The 15-char ceiling is IFNAMSIZ-1 (kernel limit).
var linuxBridgeNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`)

// ValidateNetworkType returns nil when t is a recognised
// store.NetworkType value. The set is anchored to the SQL enum;
// adding a new type means extending both the migration and this
// branch.
func ValidateNetworkType(t string) error {
	if store.NetworkType(t) == store.NetworkTypeBridge {
		return nil
	}
	return fmt.Errorf("invalid network type %q (must be one of: bridge)", t)
}

// ValidateBridgeName returns nil when s is a syntactically valid
// Linux bridge name. The kernel imposes IFNAMSIZ=16 (15 + NUL); the
// regex enforces the practical alphabet.
func ValidateBridgeName(s string) error {
	if s == "" {
		return errors.New("bridge_name is required")
	}
	if !linuxBridgeNameRe.MatchString(s) {
		return fmt.Errorf("invalid bridge_name %q (1..15 chars, [A-Za-z][A-Za-z0-9_-]*)", s)
	}
	return nil
}

// ValidateVLANTag returns nil when v is in the IEEE 802.1Q range.
// 0 and 4095 are reserved (untagged and reserved respectively); this
// matches the schema CHECK and the OpenAPI bound.
func ValidateVLANTag(v int) error {
	if v < MinVLANTag || v > MaxVLANTag {
		return fmt.Errorf("invalid vlan_tag %d (must be between %d and %d)", v, MinVLANTag, MaxVLANTag)
	}
	return nil
}

// ValidateMTU returns nil when v is between MinMTU and MaxMTU
// inclusive. Common values: 1500 (Ethernet), 1450 (VPN/overlay
// after IPSec/Geneve overhead), 9000 (jumbo frames).
func ValidateMTU(v int) error {
	if v < MinMTU || v > MaxMTU {
		return fmt.Errorf("invalid mtu %d (must be between %d and %d)", v, MinMTU, MaxMTU)
	}
	return nil
}
