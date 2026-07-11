// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package zram

import "os"

// Observe reports the node's active zram compressed-swap net (the largest zram
// swap device), or nil when none is active. It reads /proc/swaps and
// /sys/block/zramN directly - both world-readable - so it works under the
// unprivileged agent. It never shells out to swapon --show (which resolves
// labels via blkid, opening /dev/zramN which the non-root agent cannot). It
// never errors: a read failure degrades to nil (off).
func Observe() *Active {
	b, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return nil
	}
	return observeLargest(parseZramSwapDevices(string(b)), "/sys")
}
