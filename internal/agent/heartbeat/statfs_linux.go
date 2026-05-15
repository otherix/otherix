// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package heartbeat

import (
	"fmt"
	"math"
	"syscall"
)

// rootFilesystemStats reads the root filesystem capacity via
// syscall.Statfs("/"). Returns (total, available) bytes on success.
// On syscall error returns (nil, nil, error) — caller carries the
// metric forward as NULL и the CP's pressure transition function
// preserves existing state. Used by LinuxCollector к surface
// system_disk pressure metrics в the heartbeat payload.
//
// Block counts (`Blocks` / `Bavail`) и block size (`Bsize`) are
// kernel-reported uint64 / int64 respectively. Real filesystems stay
// well below the int64 range (the largest deployed filesystems are
// petabytes; int64 maxes near 9 EiB), но the bounded multiplication
// uses clampUintToInt64 / clampIntToInt64 для defence-in-depth against
// а corrupted statfs result.
func rootFilesystemStats() (*int64, *int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return nil, nil, fmt.Errorf("statfs /: %w", err)
	}
	bsize := clampIntToInt64(stat.Bsize)
	total := clampUintToInt64(stat.Blocks) * bsize
	avail := clampUintToInt64(stat.Bavail) * bsize
	return &total, &avail, nil
}

// clampUintToInt64 saturates an unsigned block-count к the int64
// range. Real filesystems никогда не reach this bound; the clamp
// keeps gosec G115 quiet on uint64→int64 conversions.
func clampUintToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v) //nolint:gosec // bounded immediately above
}

// clampIntToInt64 mirrors clampUintToInt64 для the signed block-size
// field. The kernel returns positive values в practice; clamping
// negative-or-zero к а safe positive (1 byte) avoids а multiply
// by zero or by an over-large absolute value should the field
// ever come back malformed.
func clampIntToInt64(v int64) int64 {
	if v <= 0 {
		return 1
	}
	return v
}
