// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package dnsproxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// freebindControl sets IP_FREEBIND on the listening socket so it can bind a
// local address (the overlay anycast gateway) that may not yet be configured
// on any interface when the forwarder starts.
func freebindControl(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
	}); err != nil {
		return err
	}
	return serr
}
