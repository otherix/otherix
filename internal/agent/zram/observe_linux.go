// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package zram

import "os/exec"

// Observe reports the node's OWN active zram swap (matched by the otxzram swap
// label), or nil when none is active. It reads reality via `swapon --show`, so a
// manual swapoff or a failed setup is reflected honestly, and a distro's own
// zram (different/absent label) is never mistaken for ours. It never errors.
func Observe() *Active {
	out, err := exec.Command("swapon", "--show=NAME,LABEL", "--noheadings", "--raw").Output()
	if err != nil {
		return nil
	}
	return observeOwned(parseSwaponLabels(string(out)), "/sys", swapLabel)
}
