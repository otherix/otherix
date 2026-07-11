// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package zram provides the node's agent-owned zram compressed-swap safety
// net (memory-density slice 2). It sets up an isolated zram swap device at
// agent boot and observes the live swap state for the heartbeat. All state
// lives in the kernel (pure RAM); nothing here touches disk.
package zram

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// swapLabel is the swap LABEL that tags an otherix-owned zram device.
const swapLabel = "otxzram"

// Params is the desired zram state, mapped from config.ZramConfig by the
// caller (kept config-free so this package does not import internal/config).
type Params struct {
	Enabled       bool
	MaxRAMPercent int
	Algorithm     string
}

// Active describes an active otherix zram swap device, or is nil when none is
// active. It is derived from observed reality (/proc/swaps + /sys/block/zramN),
// not from configured intent.
type Active struct {
	Device      string
	Kind        string
	SizeMib     int64
	MemLimitMib int64
	Algorithm   string
}

// parseSwaponLabels parses `swapon --show=NAME,LABEL --noheadings --raw` output
// into a device->label map. A row with no label yields an empty-string value.
func parseSwaponLabels(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		label := ""
		if len(fields) > 1 {
			label = fields[1]
		}
		m[fields[0]] = label
	}
	return m
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

// observeOwned returns the Active for the /dev/zramN device whose swap label is
// wantLabel (our ownership tag), reading its attributes from sysRoot/block/zramN.
// Returns nil when no owned zram swap is present - a distro zram with a different
// or absent label is deliberately ignored. Attribute-read failures degrade to
// zero fields rather than erroring (observation must never block the heartbeat).
func observeOwned(labels map[string]string, sysRoot, wantLabel string) *Active {
	for device, label := range labels {
		if label != wantLabel {
			continue
		}
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
		return a
	}
	return nil
}
