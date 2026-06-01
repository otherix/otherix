// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package netfabric wraps Linux host networking (bridges, taps, NAT)
// behind a small testable interface so the agent's VM manager can attach
// VMs to Linux bridges without depending on netlink directly. The
// concrete implementation is Linux-only; on other platforms New returns
// an unsupported stub so the agent still cross-compiles on developer
// machines.
package netfabric

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/google/uuid"
)

// errUnsupported is returned by every method on non-Linux builds and by
// the not-yet-implemented Linux tap/NAT primitives. Callers may
// errors.Is against it to detect that a platform or task does not
// support the requested operation.
var errUnsupported = errors.New("netfabric: unsupported on this platform")

// allowedModels is the set of QEMU NIC models the fabric recognises.
var allowedModels = map[string]struct{}{
	"virtio":  {},
	"e1000":   {},
	"e1000e":  {},
	"rtl8139": {},
}

// Fabric materialises and tears down host networking primitives for VM
// interfaces. Implementations are not safe for concurrent use unless
// documented otherwise; the VM manager serialises calls per node.
type Fabric interface {
	// EnsureBridge creates the named Linux bridge if absent, sets its MTU
	// when mtu is positive, and brings it up. It is idempotent.
	EnsureBridge(name string, mtu int) error
	// RemoveBridge deletes the named bridge. It returns nil if the bridge
	// is already absent.
	RemoveBridge(name string) error
	// BridgeExists reports whether a bridge with the given name exists.
	BridgeExists(name string) (bool, error)

	// CreateTap creates a tap device with the given name and MTU.
	CreateTap(name string, mtu int) error
	// AttachTap enslaves the tap device to the named bridge.
	AttachTap(tap, bridge string) error
	// DeleteTap removes the named tap device.
	DeleteTap(name string) error

	// EnsureGatewayAddr assigns addr to the named bridge, idempotently.
	EnsureGatewayAddr(bridge string, addr netip.Prefix) error
	// RemoveGatewayAddr removes addr from the named bridge.
	RemoveGatewayAddr(bridge string, addr netip.Prefix) error
	// EnsureMasquerade installs a masquerade rule for subnet egressing via
	// egressIface, idempotently.
	EnsureMasquerade(subnet netip.Prefix, egressIface string) error
	// RemoveMasquerade removes the masquerade rule for subnet.
	RemoveMasquerade(subnet netip.Prefix) error
}

// NIC is one VM network interface to materialise. It is shared by the
// later VM-wiring tasks and carries everything the fabric needs to
// create the host-side tap and the guest-side device.
type NIC struct {
	ID          uuid.UUID
	Bridge      string
	MAC         string
	Model       string
	MTU         int
	DeviceOrder int
}

// TapName returns the deterministic host tap interface name for the NIC:
// "ot" followed by the first 12 hex digits of the NIC id (14 chars
// total, within the Linux IFNAMSIZ-1 limit of 15).
func (n NIC) TapName() string {
	hex := n.ID.String()[:8] + n.ID.String()[9:13]
	return "ot" + hex
}

// ValidateMAC reports whether s is a valid 6-octet (EUI-48) MAC address.
// Empty strings and longer forms such as EUI-64 are rejected.
func ValidateMAC(s string) error {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return fmt.Errorf("netfabric: invalid MAC %q: %v", s, err)
	}
	if len(hw) != 6 {
		return fmt.Errorf("netfabric: MAC %q is not a 6-octet address", s)
	}
	return nil
}

// ValidateModel reports whether s names a supported QEMU NIC model. An
// empty string is accepted; a default is applied later by the caller.
func ValidateModel(s string) error {
	if s == "" {
		return nil
	}
	if _, ok := allowedModels[s]; !ok {
		return fmt.Errorf("netfabric: unsupported NIC model %q", s)
	}
	return nil
}
