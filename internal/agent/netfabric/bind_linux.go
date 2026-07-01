// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package netfabric

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// BindToDeviceControl returns a net.Dialer Control function that binds the
// dialing socket to the named device with SO_BINDTODEVICE, so the datagram or
// connection egresses that interface regardless of the main route table. Two
// overlays can carry the same guest subnet on different bridges; a route-based
// send would otherwise leave the wrong bridge and reach a different tenant. The
// agent and gateway run as root, so the socket carries CAP_NET_RAW. A blank
// device yields an unbound socket.
func BindToDeviceControl(device string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		if device == "" {
			return nil
		}
		var serr error
		if cerr := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
		}); cerr != nil {
			return cerr
		}
		return serr
	}
}
