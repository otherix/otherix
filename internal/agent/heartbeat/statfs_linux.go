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
// metric forward as NULL and the CP's pressure transition function
// preserves existing state. Used by LinuxCollector to surface
// system_disk pressure metrics in the heartbeat payload.
//
// Block counts (`Blocks` / `Bavail`) and block size (`Bsize`) are
// kernel-reported uint64 / int64 respectively. Real filesystems stay
// well below the int64 range (the largest deployed filesystems are
// petabytes; int64 maxes near 9 EiB), but the bounded multiplication
// uses clampUintToInt64 / clampIntToInt64 for defence-in-depth against
// a corrupted statfs result.
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

// clampUintToInt64 saturates an unsigned block-count to the int64
// range. Real filesystems never not reach this bound; the clamp
// keeps gosec G115 quiet on uint64→int64 conversions.
func clampUintToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v) //nolint:gosec // bounded immediately above
}

// clampIntToInt64 mirrors clampUintToInt64 for the signed block-size
// field. The kernel returns positive values in practice; clamping
// negative-or-zero to a safe positive (1 byte) avoids a multiply
// by zero or by an over-large absolute value should the field
// ever come back malformed.
func clampIntToInt64(v int64) int64 {
	if v <= 0 {
		return 1
	}
	return v
}
