// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package qemu

import "errors"

// OpenPidfd is unavailable off Linux; the agent is Linux-only, and a sweep
// elsewhere opens nothing.
func OpenPidfd(int) (ProcessHandle, error) {
	return nil, errors.New("pidfd is linux-only")
}
