// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import (
	"net"
	"net/netip"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// buildReply builds the DHCPv4 reply for req given the matched reservation and
// the network subnet. A DISCOVER yields an OFFER; a REQUEST whose requested or
// bound address equals the reservation yields an ACK, otherwise a NAK. It sets
// yiaddr, the subnet mask (option 1), DNS (option 6), the server identifier
// (option 54), the lease time (option 51), and option 121 (classless static
// routes: gateway/32 on-link + default via gateway). Option 3 (router) is NOT
// set - the link-local gateway is out-of-subnet and reached via option 121.
func buildReply(req *dhcpv4.DHCPv4, res Reservation, subnet netip.Prefix, opt ReplyOptions) (*dhcpv4.DHCPv4, error) {
	gateway := ip4(opt.Gateway)
	yourIP := ip4(res.IP)

	msgType, ack := replyType(req, res)
	reply, err := dhcpv4.NewReplyFromRequest(req, dhcpv4.WithMessageType(msgType))
	if err != nil {
		return nil, err
	}

	// A NAK carries no client configuration; only the server identifier is
	// meaningful so the client can match the rejecting server.
	reply.UpdateOption(dhcpv4.OptServerIdentifier(gateway))
	if !ack {
		return reply, nil
	}

	reply.YourIPAddr = yourIP
	reply.UpdateOption(dhcpv4.OptSubnetMask(net.CIDRMask(subnet.Bits(), 32)))
	reply.UpdateOption(dhcpv4.OptDNS(ip4(opt.DNS)))
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(opt.Lease))

	// Option 121: the link-local gateway is out-of-subnet, so deliver the
	// default route via classless static routes (RFC 3442). The gateway is
	// reachable on-link (zero next-hop), and the default route uses it.
	onLink := &dhcpv4.Route{
		Dest:   &net.IPNet{IP: gateway, Mask: net.CIDRMask(32, 32)},
		Router: net.IPv4zero.To4(),
	}
	defaultRoute := &dhcpv4.Route{
		Dest:   &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
		Router: gateway,
	}
	reply.UpdateOption(dhcpv4.OptClasslessStaticRoute(onLink, defaultRoute))

	return reply, nil
}

// replyType returns the reply message type for req and whether it acknowledges
// the reservation. A DISCOVER offers; a REQUEST acks when the requested address
// (or, absent that, the bound client address) is unset or equals the
// reservation, and naks otherwise.
func replyType(req *dhcpv4.DHCPv4, res Reservation) (dhcpv4.MessageType, bool) {
	if req.MessageType() == dhcpv4.MessageTypeDiscover {
		return dhcpv4.MessageTypeOffer, true
	}

	want := ip4(res.IP)
	if requested := req.RequestedIPAddress().To4(); requested != nil && !requested.Equal(net.IPv4zero.To4()) {
		if requested.Equal(want) {
			return dhcpv4.MessageTypeAck, true
		}
		return dhcpv4.MessageTypeNak, false
	}
	if bound := req.ClientIPAddr.To4(); bound != nil && !bound.Equal(net.IPv4zero.To4()) {
		if bound.Equal(want) {
			return dhcpv4.MessageTypeAck, true
		}
		return dhcpv4.MessageTypeNak, false
	}
	return dhcpv4.MessageTypeAck, true
}

// ip4 converts a netip.Addr to a 4-byte net.IP.
func ip4(a netip.Addr) net.IP {
	b := a.As4()
	return net.IP(b[:])
}
