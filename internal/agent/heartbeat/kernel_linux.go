// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package heartbeat

import "syscall"

// kernelReleaseFromUname returns the kernel release string from the
// Linux uname syscall (e.g. "6.6.0-otherix"). Empty on syscall error;
// caller falls back to /proc/version.
func kernelReleaseFromUname() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	return utsnameReleaseString(u.Release)
}

// utsnameReleaseString converts the syscall.Utsname.Release byte array
// to a Go string, terminating at the first NUL. Generic over int8 and
// uint8 because the underlying type differs between linux/amd64
// (int8) and linux/arm64 (uint8).
func utsnameReleaseString[T ~int8 | ~uint8](buf [65]T) string {
	out := make([]byte, 0, len(buf))
	for _, c := range buf {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
