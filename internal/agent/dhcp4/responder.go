// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package dhcp4 implements a per-node DHCPv4 responder for overlay VMs. It
// answers only known MACs (CP-IPAM reservations) on dhcp-enabled overlay
// bridges, handing each VM its reserved IP, the subnet mask, the overlay
// anycast resolver, and a default route via DHCP option 121 (the link-local
// gateway is out-of-subnet, so option 3 is intentionally never set).
package dhcp4

import (
	"net"
	"net/netip"
	"time"
)

// Reservation is one MAC->IPv4 binding served on a dhcp-enabled overlay network.
type Reservation struct {
	MAC net.HardwareAddr
	IP  netip.Addr
}

// NetworkConfig is the DHCP service descriptor for one managed-DHCP network's
// bridge (an overlay bridge or a managed bridge - the responder is the same).
type NetworkConfig struct {
	NetworkID    string
	Bridge       string
	Subnet       netip.Prefix
	Reservations []Reservation

	// AdvertiseDNS controls DHCP option 6 (the overlay resolver). AdvertiseDefaultRoute
	// controls the option-121 default route (0.0.0.0/0). Both are set per network by
	// the reconciler.
	AdvertiseDNS          bool
	AdvertiseDefaultRoute bool
}

// Responder serves DHCPv4 for CP-IPAM reservations on any managed-DHCP network's
// bridge - an overlay bridge or a managed bridge alike (both register through the
// same reconciler step). The reconciler registers/deregisters per network; the
// implementation owns one raw socket per active bridge.
type Responder interface {
	RegisterNetwork(cfg NetworkConfig) error
	DeregisterNetwork(networkID string) error
	// LookupByMAC returns the lease IP this agent serves for mac across all
	// active bridges, canonicalizing mac via net.ParseMAC. It is the agent's
	// own DHCP state and the ssh-pipe handler's anti-SSRF IP source.
	LookupByMAC(mac string) (netip.Addr, bool)
}

// ReplyOptions are the anycast constants every reply carries (overlay gateway).
type ReplyOptions struct {
	Gateway netip.Addr
	DNS     netip.Addr
	Lease   time.Duration
}

// DefaultLease is the lease time handed to clients. Reservations are stable, so
// a short renewable lease is fine; the client re-confirms the same binding.
const DefaultLease = time.Hour
