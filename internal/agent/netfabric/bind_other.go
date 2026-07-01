// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package netfabric

import "syscall"

// BindToDeviceControl is a no-op on non-Linux builds: SO_BINDTODEVICE is
// Linux-only, and the agent and gateway run on Linux. The development build on
// other platforms gets an unbound dialer.
func BindToDeviceControl(_ string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error { return nil }
}
