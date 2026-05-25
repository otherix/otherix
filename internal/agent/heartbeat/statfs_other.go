// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package heartbeat

// rootFilesystemStats is a no-op stub for non-Linux builds. The
// agent's LinuxCollector constructor (heartbeat.NewLinux) rejects
// non-Linux GOOS at runtime, so this path is reachable only during
// cross-compile / unit tests on developer machines, never in a live
// agent process. Returns (nil, nil, nil) — caller silently treats
// the metric as "not reported" and the CP receiver carries existing
// pressure state forward.
func rootFilesystemStats() (*int64, *int64, error) {
	return nil, nil, nil
}
