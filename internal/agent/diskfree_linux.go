// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package agent

import (
	"fmt"
	"syscall"
)

// freeBytesStatfs returns the bytes available to an unprivileged writer on the
// filesystem backing path. Used by the image cache eviction sweeper to enforce
// the free-space floor.
func freeBytesStatfs(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %v", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // G115: Bsize is the kernel block size, always small and positive
}
