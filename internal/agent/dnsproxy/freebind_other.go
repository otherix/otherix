// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package dnsproxy

import "syscall"

// freebindControl is a no-op on non-Linux platforms (IP_FREEBIND is
// Linux-specific). The forwarder only runs for real on the Linux agent; this
// stub lets the package build and unit-test on developer machines, where the
// test binds a loopback address that already exists.
func freebindControl(_, _ string, _ syscall.RawConn) error { return nil }
