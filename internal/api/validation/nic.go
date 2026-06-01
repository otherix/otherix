// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"net"
)

// MaxDeviceOrder bounds a NIC's slot index within a VM. A small ceiling
// keeps the device order to a sane number of interfaces per VM.
const MaxDeviceOrder = 15

// AllowedNICModels is the set of QEMU NIC models accepted at the
// control-plane edge. It is an independent copy of the agent's
// netfabric allow-list (internal/agent/netfabric.allowedModels), kept
// in sync by hand: the validation package must not depend on agent
// code, so the small set is duplicated rather than imported. Keep this
// set identical to netfabric's when either side changes.
var AllowedNICModels = map[string]struct{}{
	"virtio":  {},
	"e1000":   {},
	"e1000e":  {},
	"rtl8139": {},
}

// ValidateNICModel returns nil when s names a supported QEMU NIC model.
// The empty string is accepted; the caller applies a default later.
func ValidateNICModel(s string) error {
	if s == "" {
		return nil
	}
	if _, ok := AllowedNICModels[s]; !ok {
		return fmt.Errorf("unsupported NIC model %q", s)
	}
	return nil
}

// ValidateNICMAC returns nil when s is a non-empty 6-octet (EUI-48) MAC
// address. Empty strings and longer forms such as EUI-64 are rejected.
// It mirrors netfabric.ValidateMAC.
func ValidateNICMAC(s string) error {
	if s == "" {
		return errors.New("mac is required")
	}
	hw, err := net.ParseMAC(s)
	if err != nil {
		return fmt.Errorf("invalid mac %q: %v", s, err)
	}
	if len(hw) != 6 {
		return fmt.Errorf("invalid mac %q: not a 6-octet address", s)
	}
	return nil
}

// ValidateDeviceOrder returns nil when n is a valid NIC slot index,
// 0..MaxDeviceOrder inclusive.
func ValidateDeviceOrder(n int) error {
	if n < 0 || n > MaxDeviceOrder {
		return fmt.Errorf("invalid device_order %d (must be between 0 and %d)", n, MaxDeviceOrder)
	}
	return nil
}
