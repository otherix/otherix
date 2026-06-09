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

// NetworkConfig is the DHCP service descriptor for one overlay bridge.
type NetworkConfig struct {
	NetworkID    string
	Bridge       string
	Subnet       netip.Prefix
	Reservations []Reservation
}

// Responder serves DHCPv4 on overlay bridges for CP-IPAM reservations. The
// reconciler registers/deregisters per network; the implementation owns one raw
// socket per active bridge (added in a later task).
type Responder interface {
	RegisterNetwork(cfg NetworkConfig) error
	DeregisterNetwork(networkID string) error
}

// ReplyOptions are the anycast constants every reply carries (Slice 1 gateway).
type ReplyOptions struct {
	Gateway netip.Addr
	DNS     netip.Addr
	Lease   time.Duration
}

// DefaultLease is the lease time handed to clients. Reservations are stable, so
// a short renewable lease is fine; the client re-confirms the same binding.
const DefaultLease = time.Hour
