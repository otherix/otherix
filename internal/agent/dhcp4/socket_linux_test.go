// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package dhcp4

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestIPChecksumKnownVector(t *testing.T) {
	// RFC 1071-style worked example header (checksum field zeroed) from the
	// classic UDP/IP derivation yields 0xb861.
	header := []byte{
		0x45, 0x00, 0x00, 0x73, 0x00, 0x00, 0x40, 0x00,
		0x40, 0x11, 0x00, 0x00, 0xc0, 0xa8, 0x00, 0x01,
		0xc0, 0xa8, 0x00, 0xc7,
	}
	if got := ipChecksum(header); got != 0xb861 {
		t.Errorf("ipChecksum = %#04x, want 0xb861", got)
	}
}

func TestBuildFrameLayout(t *testing.T) {
	src, _ := net.ParseMAC("02:00:00:00:00:01")
	dst, _ := net.ParseMAC("52:54:00:aa:bb:cc")
	payload := []byte("dhcp-payload-bytes")

	frame := buildFrame(src, dst, payload)

	wantLen := ethHeaderLen + ipHeaderLen + udpHeaderLen + len(payload)
	if len(frame) != wantLen {
		t.Fatalf("frame length = %d, want %d", len(frame), wantLen)
	}

	// Ethernet destination and source.
	if got := net.HardwareAddr(frame[0:6]).String(); got != dst.String() {
		t.Errorf("eth dst = %v, want %v", got, dst)
	}
	if got := net.HardwareAddr(frame[6:12]).String(); got != src.String() {
		t.Errorf("eth src = %v, want %v", got, src)
	}
	if got := binary.BigEndian.Uint16(frame[12:14]); got != 0x0800 {
		t.Errorf("ethertype = %#04x, want 0x0800", got)
	}

	// IPv4 header checksum must verify: ones-complement sum over the filled
	// header (including the checksum field) is zero.
	ip := frame[ethHeaderLen : ethHeaderLen+ipHeaderLen]
	if got := ipChecksum(ip); got != 0 {
		t.Errorf("ipChecksum over filled header = %#04x, want 0 (valid)", got)
	}
	// Source 169.254.1.1, destination 255.255.255.255.
	if got := net.IP(ip[12:16]).String(); got != "169.254.1.1" {
		t.Errorf("ip src = %v, want 169.254.1.1", got)
	}
	if got := net.IP(ip[16:20]).String(); got != "255.255.255.255" {
		t.Errorf("ip dst = %v, want 255.255.255.255", got)
	}

	// UDP: server source port 67, client dest port 68, correct length.
	udp := frame[ethHeaderLen+ipHeaderLen:]
	if got := binary.BigEndian.Uint16(udp[0:2]); got != dhcpServerPort {
		t.Errorf("udp src port = %d, want %d", got, dhcpServerPort)
	}
	if got := binary.BigEndian.Uint16(udp[2:4]); got != dhcpClientPort {
		t.Errorf("udp dst port = %d, want %d", got, dhcpClientPort)
	}
	if got := int(binary.BigEndian.Uint16(udp[4:6])); got != udpHeaderLen+len(payload) {
		t.Errorf("udp length = %d, want %d", got, udpHeaderLen+len(payload))
	}
	if got := string(udp[udpHeaderLen:]); got != string(payload) {
		t.Errorf("udp payload = %q, want %q", got, payload)
	}
}

// requestFrame builds an inbound client->server DHCP request frame (UDP dst 67)
// so the parser has something to recover.
func requestFrame(srcMAC net.HardwareAddr, payload []byte) []byte {
	udpLen := udpHeaderLen + len(payload)
	ipLen := ipHeaderLen + udpLen
	frame := make([]byte, ethHeaderLen+ipLen)
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	ip := frame[ethHeaderLen:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipLen))
	ip[9] = 17 // UDP
	udp := ip[ipHeaderLen:]
	binary.BigEndian.PutUint16(udp[0:2], dhcpClientPort)
	binary.BigEndian.PutUint16(udp[2:4], dhcpServerPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[udpHeaderLen:], payload)
	return frame
}

func TestParseDHCPFrameRecoversRequest(t *testing.T) {
	src, _ := net.ParseMAC("52:54:00:aa:bb:cc")
	payload := []byte("client-discover")
	frame := requestFrame(src, payload)

	gotPayload, ok := parseDHCPFrame(frame)
	if !ok {
		t.Fatal("parseDHCPFrame returned ok=false for a well-formed request")
	}
	if string(gotPayload) != string(payload) {
		t.Errorf("payload = %q, want %q", gotPayload, payload)
	}
}

func TestParseDHCPFrameRejectsNonDHCP(t *testing.T) {
	src, _ := net.ParseMAC("52:54:00:aa:bb:cc")
	frame := requestFrame(src, []byte("x"))
	// Rewrite UDP dst port from 67 to 53: parseDHCPFrame must reject it.
	udp := frame[ethHeaderLen+ipHeaderLen:]
	binary.BigEndian.PutUint16(udp[2:4], 53)

	if _, ok := parseDHCPFrame(frame); ok {
		t.Error("parseDHCPFrame accepted a non-DHCP (port 53) frame")
	}
}

func TestHtons(t *testing.T) {
	if got := htons(0x0800); got != 0x0008 {
		t.Errorf("htons(0x0800) = %#04x, want 0x0008", got)
	}
}
