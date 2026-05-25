// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package vm

import (
	"fmt"
	"math"
	"syscall"
)

// pathFilesystemStats reports total and available bytes of the filesystem
// containing path via syscall.Statfs. Mirrors the pattern in
// internal/agent/heartbeat/statfs_linux.go (rootFilesystemStats) —
// duplicated rather than relocated to avoid touching the heartbeat
// package during the A1 fix; consolidation under a shared
// internal/agent/fsutil/ package is tracked in ROADMAP.
//
// Returns (nil, nil, error) on syscall failure — caller surfaces the
// failure as a terminal-failed agent task. clampUintToInt64 /
// clampIntToInt64 keep gosec G115 quiet on the kernel's
// uint64/int64 block-count and block-size fields.
func pathFilesystemStats(path string) (*int64, *int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, nil, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := clampIntToInt64(stat.Bsize)
	total := clampUintToInt64(stat.Blocks) * bsize
	avail := clampUintToInt64(stat.Bavail) * bsize
	return &total, &avail, nil
}

// clampUintToInt64 saturates an unsigned block-count to the int64
// range. Real filesystems never reach this bound; the clamp keeps
// gosec G115 quiet on uint64→int64 conversions.
func clampUintToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v) //nolint:gosec // bounded immediately above
}

// clampIntToInt64 normalises the signed block-size field to a strictly
// positive value. The kernel returns positive values in practice;
// clamping non-positive to 1 byte avoids a multiply by zero or by an
// over-large absolute value should the field ever come back malformed.
func clampIntToInt64(v int64) int64 {
	if v <= 0 {
		return 1
	}
	return v
}
