// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import (
	"bytes"
	"net"
	"net/netip"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q) = %v", s, err)
	}
	return m
}

func testReservation(t *testing.T) (Reservation, netip.Prefix, ReplyOptions) {
	t.Helper()
	res := Reservation{
		MAC: mustMAC(t, "52:54:00:12:34:56"),
		IP:  netip.MustParseAddr("10.20.0.5"),
	}
	subnet := netip.MustParsePrefix("10.20.0.0/24")
	opt := ReplyOptions{
		Gateway: netfabric.OverlayGatewayAddr,
		DNS:     netfabric.OverlayGatewayAddr,
		Lease:   DefaultLease,
	}
	return res, subnet, opt
}

func newRequest(t *testing.T, mac net.HardwareAddr, mt dhcpv4.MessageType, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	all := append([]dhcpv4.Modifier{dhcpv4.WithHwAddr(mac), dhcpv4.WithMessageType(mt)}, mods...)
	req, err := dhcpv4.New(all...)
	if err != nil {
		t.Fatalf("dhcpv4.New() = %v", err)
	}
	return req
}

func TestBuildReplyDiscoverYieldsOffer(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if got := reply.MessageType(); got != dhcpv4.MessageTypeOffer {
		t.Errorf("MessageType = %v, want OFFER", got)
	}
	if got := reply.YourIPAddr.String(); got != "10.20.0.5" {
		t.Errorf("YourIPAddr = %v, want 10.20.0.5", got)
	}
}

func TestBuildReplyNetmask(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	want := net.CIDRMask(subnet.Bits(), 32)
	if got := reply.SubnetMask(); !bytes.Equal(got, want) {
		t.Errorf("SubnetMask = %v, want %v", got, want)
	}
}

func TestBuildReplyDNS(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	dns := reply.DNS()
	if len(dns) != 1 {
		t.Fatalf("DNS() = %v, want exactly one server", dns)
	}
	if got := dns[0].String(); got != "169.254.1.1" {
		t.Errorf("DNS = %v, want 169.254.1.1", got)
	}
}

func TestBuildReplyServerIdentifier(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if got := reply.ServerIdentifier().String(); got != "169.254.1.1" {
		t.Errorf("ServerIdentifier = %v, want 169.254.1.1", got)
	}
}

func TestBuildReplyLeaseTime(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if got := reply.IPAddressLeaseTime(0); got != DefaultLease {
		t.Errorf("IPAddressLeaseTime = %v, want %v", got, DefaultLease)
	}
}

func TestBuildReplyClasslessStaticRoute(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	routes := reply.ClasslessStaticRoute()
	if len(routes) != 2 {
		t.Fatalf("ClasslessStaticRoute() = %v, want 2 routes", routes)
	}

	// Route 0: 169.254.1.1/32 on-link (zero next-hop).
	if got := routes[0].Dest.String(); got != "169.254.1.1/32" {
		t.Errorf("route[0].Dest = %v, want 169.254.1.1/32", got)
	}
	if !routes[0].Router.Equal(net.IPv4zero) {
		t.Errorf("route[0].Router = %v, want 0.0.0.0 (on-link)", routes[0].Router)
	}

	// Route 1: default 0.0.0.0/0 via 169.254.1.1.
	if got := routes[1].Dest.String(); got != "0.0.0.0/0" {
		t.Errorf("route[1].Dest = %v, want 0.0.0.0/0", got)
	}
	if got := routes[1].Router.String(); got != "169.254.1.1" {
		t.Errorf("route[1].Router = %v, want 169.254.1.1", got)
	}
}

// TestBuildReplyOption121RawBytes asserts the raw RFC-3442 option-121 wire
// bytes independent of the library decoder. TestBuildReplyClasslessStaticRoute
// round-trips through the library's own getter, which shares Marshal/Unmarshal
// with the encoder and would mask a symmetric encoding bug; this test pins the
// literal wire form so a regression in the encoding fails loudly.
func TestBuildReplyOption121RawBytes(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	// RFC 3442 destination descriptor: significant-octet-count prefix, then the
	// significant octets of the destination, then the 4-byte router.
	//   20 a9 fe 01 01 00 00 00 00  -> 169.254.1.1/32 on-link, router 0.0.0.0
	//   00 a9 fe 01 01              -> 0.0.0.0/0 (no significant octets) via
	//                                  169.254.1.1
	want := []byte{
		0x20, 0xa9, 0xfe, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0xa9, 0xfe, 0x01, 0x01,
	}
	got := reply.Options.Get(dhcpv4.OptionClasslessStaticRoute)
	if !bytes.Equal(got, want) {
		t.Errorf("option 121 = % x, want % x", got, want)
	}
}

func TestBuildReplyConditionalOptions(t *testing.T) {
	cases := []struct {
		name             string
		advertiseDNS     bool
		advertiseDefault bool
		wantDNS          bool
		wantOnLink       bool
		wantDefaultRoute bool
	}{
		{name: "nat style dns+default", advertiseDNS: true, advertiseDefault: true, wantDNS: true, wantOnLink: true, wantDefaultRoute: true},
		{name: "isolated dns only", advertiseDNS: true, advertiseDefault: false, wantDNS: true, wantOnLink: true, wantDefaultRoute: false},
		{name: "addressing only", advertiseDNS: false, advertiseDefault: false, wantDNS: false, wantOnLink: false, wantDefaultRoute: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, subnet, opt := testReservation(t)
			req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)
			reply, err := buildReply(req, res, subnet, opt, tc.advertiseDNS, tc.advertiseDefault)
			if err != nil {
				t.Fatalf("buildReply: %v", err)
			}
			if gotDNS := len(reply.DNS()) > 0; gotDNS != tc.wantDNS {
				t.Errorf("opt6 DNS present = %v, want %v", gotDNS, tc.wantDNS)
			}
			var onLink, def bool
			for _, rt := range reply.ClasslessStaticRoute() {
				if ones, _ := rt.Dest.Mask.Size(); ones == 32 {
					onLink = true
				} else if ones == 0 {
					def = true
				}
			}
			if onLink != tc.wantOnLink {
				t.Errorf("opt121 on-link = %v, want %v", onLink, tc.wantOnLink)
			}
			if def != tc.wantDefaultRoute {
				t.Errorf("opt121 default = %v, want %v", def, tc.wantDefaultRoute)
			}
		})
	}
}

func TestBuildReplyRejectsNonIPv4Reservation(t *testing.T) {
	res, subnet, opt := testReservation(t)
	res.IP = netip.MustParseAddr("fd00::5")
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	if _, err := buildReply(req, res, subnet, opt, true, true); err == nil {
		t.Fatalf("buildReply() with IPv6 reservation = nil error, want error")
	}
}

func TestBuildReplyNoRouterOption(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeDiscover)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if reply.Options.Has(dhcpv4.OptionRouter) {
		t.Errorf("option 3 (router) present, want absent")
	}
	if got := reply.Router(); len(got) != 0 {
		t.Errorf("Router() = %v, want none", got)
	}
}

func TestBuildReplyRequestMatchingIPYieldsAck(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(res.IP.AsSlice())))

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if got := reply.MessageType(); got != dhcpv4.MessageTypeAck {
		t.Errorf("MessageType = %v, want ACK", got)
	}
	if got := reply.YourIPAddr.String(); got != "10.20.0.5" {
		t.Errorf("YourIPAddr = %v, want 10.20.0.5", got)
	}
}

func TestBuildReplyRequestWrongIPYieldsNak(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeRequest,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("10.20.0.99"))))

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if got := reply.MessageType(); got != dhcpv4.MessageTypeNak {
		t.Errorf("MessageType = %v, want NAK", got)
	}
}

func TestBuildReplyRequestNoRequestedIPYieldsAck(t *testing.T) {
	res, subnet, opt := testReservation(t)
	req := newRequest(t, res.MAC, dhcpv4.MessageTypeRequest)

	reply, err := buildReply(req, res, subnet, opt, true, true)
	if err != nil {
		t.Fatalf("buildReply() = %v", err)
	}

	if got := reply.MessageType(); got != dhcpv4.MessageTypeAck {
		t.Errorf("MessageType = %v, want ACK", got)
	}
}
