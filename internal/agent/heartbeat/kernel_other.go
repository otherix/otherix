// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package heartbeat

// kernelReleaseFromUname is а no-op stub for non-Linux builds. The
// agent's LinuxCollector constructor (heartbeat.NewLinux) rejects
// non-Linux GOOS at runtime, so this path is reachable only during
// cross-compile / unit tests on developer machines, never in а live
// agent process.
func kernelReleaseFromUname() string { return "" }
