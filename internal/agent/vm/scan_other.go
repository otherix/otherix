// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package vm

import "fmt"

// pathFilesystemStats is а no-op stub для non-Linux builds. The agent
// is Linux-only; cross-compile / unit-test paths on developer
// machines reach this stub. Returning an explicit error - rather than
// (nil, nil, nil) - keeps the scan task's terminal status honest: the
// operator sees `scan_failed: statfs unsupported on this platform`
// instead of а silently empty result.
func pathFilesystemStats(path string) (*int64, *int64, error) {
	return nil, nil, fmt.Errorf("statfs %s: unsupported on this platform", path)
}
