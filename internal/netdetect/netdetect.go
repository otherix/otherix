// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package netdetect resolves the host's primary routable address for
// self-advertising config (the etcd peer URL). It lets a single-node
// control plane boot with no operator-supplied address yet stay
// HA-ready: the advertised peer URL is a real routable IP from first
// boot, so growing to a multi-member cluster needs no node-1 rewrite.
package netdetect

import (
	"fmt"
	"net"
	"net/netip"
)

// peerPort is the etcd Raft peer port baked into an auto-resolved peer URL.
const peerPort = "2380"

// loopbackPeerURL is the fallback when no routable IPv4 exists (e.g. a
// host with only loopback). A single-node cluster stays functional on
// it, but cannot grow to HA without a later peer-URL rewrite.
const loopbackPeerURL = "https://127.0.0.1:" + peerPort

// ResolvePeerURL returns the concrete etcd peer-advertise URL. When raw
// is "auto" or empty it builds https://<addr>:2380 from detect(); any
// other value is an operator override and is returned verbatim. When
// detect reports no routable address, it falls back to loopback.
func ResolvePeerURL(raw string, detect func() (netip.Addr, bool)) (string, error) {
	if raw != "" && raw != "auto" {
		return raw, nil
	}
	addr, ok := detect()
	if !ok {
		return loopbackPeerURL, nil
	}
	if !addr.Is4() {
		return "", fmt.Errorf("detected address %s is not IPv4", addr)
	}
	return fmt.Sprintf("https://%s:%s", addr, peerPort), nil
}

// RoutableIPv4 reports the source IPv4 the kernel would use to reach a
// public destination - the host's primary routable address. It opens a
// UDP socket to a TEST-NET-1 address (RFC 5737, never routed off-link)
// and reads the chosen local address; no packets are transmitted. ok is
// false when no routable IPv4 is available.
func RoutableIPv4() (netip.Addr, bool) {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return netip.Addr{}, false
	}
	defer func() { _ = c.Close() }()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	addr, ok := netip.AddrFromSlice(ua.IP.To4())
	if !ok || !addr.IsValid() || addr.IsLoopback() || addr.IsUnspecified() {
		return netip.Addr{}, false
	}
	return addr, true
}
