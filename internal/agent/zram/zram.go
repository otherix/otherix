// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package zram observes the node's host-provided zram compressed-swap safety
// net for the heartbeat. It is READ-ONLY: it never configures, creates, or
// tears down zram - the host owns that (e.g. via systemd-zram-generator). All
// reads are of world-readable kernel files (/proc/swaps, /sys/block/zramN), so
// the agent needs no privilege.
package zram

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Active describes an active zram swap device, or is nil when none is active.
// It is derived from observed reality (/proc/swaps + /sys/block/zramN), not from
// any configured intent.
type Active struct {
	Device      string
	Kind        string
	SizeMib     int64
	MemLimitMib int64
	Algorithm   string
	// SwappedMib is the logical amount currently swapped in (uncompressed bytes
	// of data zram holds, from mm_stat orig_data_size). RAMUsedMib is the physical
	// RAM zram has allocated to store it (mm_stat mem_used_total, incl. allocator
	// overhead). Both are 0 when mm_stat is unreadable/unparseable - observation
	// never blocks the heartbeat.
	SwappedMib int64
	RAMUsedMib int64
}

// parseMemStat parses a /sys/block/zramN/mm_stat line and returns
// (orig_data_size, mem_used_total) in bytes. ok is false when the line has
// fewer than 3 fields or a field is non-numeric.
func parseMemStat(line string) (orig, memUsed int64, ok bool) {
	f := strings.Fields(line)
	if len(f) < 3 {
		return 0, 0, false
	}
	o, err1 := strconv.ParseInt(f[0], 10, 64)
	m, err2 := strconv.ParseInt(f[2], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return o, m, true
}

// parseZramSwapDevices parses /proc/swaps and returns the Filename-column
// entries that are zram devices (/dev/zram*). The first line is a header.
func parseZramSwapDevices(procSwaps string) []string {
	var devices []string
	for i, line := range strings.Split(procSwaps, "\n") {
		if i == 0 { // header row
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "/dev/zram") {
			devices = append(devices, fields[0])
		}
	}
	return devices
}

// activeAlgorithm extracts the selected algorithm from a comp_algorithm line
// like "lzo [zstd] lz4" (the bracketed token is active).
func activeAlgorithm(line string) string {
	for _, tok := range strings.Fields(line) {
		if strings.HasPrefix(tok, "[") && strings.HasSuffix(tok, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(tok, "["), "]")
		}
	}
	return strings.TrimSpace(line)
}

func readInt64(path string) (int64, bool) {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed sysfs path under /sys/block
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// observeLargest reads each zram swap device's attributes from sysRoot/block/zramN
// and returns the Active for the one with the LARGEST disksize (the honest
// protective figure the overcommit floor keys on), or nil when devices is empty.
// Attribute-read failures degrade to zero fields rather than erroring, so
// observation never blocks the heartbeat.
func observeLargest(devices []string, sysRoot string) *Active {
	var best *Active
	for _, device := range devices {
		base := filepath.Base(device)
		if !strings.HasPrefix(base, "zram") {
			continue
		}
		dev := filepath.Join(sysRoot, "block", base)
		a := &Active{Device: device, Kind: "zram"}
		if v, ok := readInt64(filepath.Join(dev, "disksize")); ok {
			a.SizeMib = v / (1024 * 1024)
		}
		if v, ok := readInt64(filepath.Join(dev, "mem_limit")); ok {
			a.MemLimitMib = v / (1024 * 1024)
		}
		if b, err := os.ReadFile(filepath.Join(dev, "comp_algorithm")); err == nil { // #nosec G304
			a.Algorithm = activeAlgorithm(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(dev, "mm_stat")); err == nil { // #nosec G304
			if orig, memUsed, ok := parseMemStat(string(b)); ok {
				a.SwappedMib = orig / (1024 * 1024)
				a.RAMUsedMib = memUsed / (1024 * 1024)
			}
		}
		if best == nil || a.SizeMib > best.SizeMib {
			best = a
		}
	}
	return best
}
