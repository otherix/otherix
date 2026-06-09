// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package dhcp4

import "errors"

// openAFPacket is the non-Linux stub. AF_PACKET is Linux-only; the agent runs
// only on Linux, so the real opener never executes off Linux. This stub lets
// the package build and unit-test on developer machines, where the responder is
// driven through an injected fake packetConn.
func openAFPacket(_ string) (packetConn, error) {
	return nil, errors.New("dhcp4: AF_PACKET is linux-only")
}
