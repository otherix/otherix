// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package agent

import "fmt"

// freeBytesStatfs is unsupported off linux (the agent runtime is linux-only).
// The image cache sweeper injects this as a seam, so non-linux unit tests pass a
// stub and never reach here; it exists only to keep the package cross-compilable.
//
//nolint:unused // platform stub: the linux build and the eviction sweeper are the real callers; this exists only to keep the package cross-compilable off linux.
func freeBytesStatfs(string) (uint64, error) {
	return 0, fmt.Errorf("statfs not supported on this platform")
}
