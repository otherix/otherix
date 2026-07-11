// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package zram provides the node's agent-owned zram compressed-swap safety
// net. It sets up an isolated zram swap device at agent boot and observes the
// live swap state for the heartbeat. All state lives in the kernel (pure RAM);
// nothing here touches disk.
package zram

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// swapLabel is the swap LABEL that tags an otherix-owned zram device.
const swapLabel = "otxzram"

// swapPriority is the swapon priority for the otherix zram device.
const swapPriority = 100

// Params is the desired zram state, mapped from config.ZramConfig by the
// caller (kept config-free so this package does not import internal/config).
type Params struct {
	Enabled       bool
	MaxRAMPercent int
	Algorithm     string
}

// Active describes an active otherix zram swap device, or is nil when none is
// active. It is derived from observed reality (swapon --show + /sys/block/zramN),
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

// hostOps is the seam over the privileged host operations Ensure performs, so
// the orchestration is unit-testable without a real kernel.
type hostOps interface {
	modprobe() error
	hotAdd() (int, error)                      // read /sys/class/zram-control/hot_add -> device id
	hotRemove(id int) error                    // write /sys/class/zram-control/hot_remove
	writeAttr(dev int, attr, val string) error // write /sys/block/zram<dev>/<attr>
	readAttr(dev int, attr string) (string, error)
	mkswap(dev int, label string) error // mkswap -L <label> /dev/zram<dev>
	swapon(dev, prio int) error
	swapoff(dev int) error
}

// devNum extracts N from "/dev/zramN". Returns -1 when unparseable.
func devNum(device string) int {
	base := filepath.Base(device)
	n, err := strconv.Atoi(strings.TrimPrefix(base, "zram"))
	if err != nil {
		return -1
	}
	return n
}

// ensureWith reconciles the node to the desired zram state. existing is the
// currently-observed otherix zram (nil if none). ramBytes is host RAM total.
// Enabled=false tears down existing; Enabled=true sets up a fresh device when
// none is active and no-ops when one already is.
func ensureWith(p Params, ops hostOps, ramBytes int64, existing *Active) (*Active, error) {
	if !p.Enabled {
		if existing == nil {
			return nil, nil
		}
		if err := ops.swapoff(devNum(existing.Device)); err != nil {
			return existing, fmt.Errorf("zram swapoff %s: %w", existing.Device, err)
		}
		if err := ops.hotRemove(devNum(existing.Device)); err != nil {
			return nil, fmt.Errorf("zram hot_remove %s: %w", existing.Device, err)
		}
		return nil, nil
	}

	if existing != nil {
		// Already active - do not add a second device (idempotent restart).
		return existing, nil
	}

	if err := ops.modprobe(); err != nil {
		return nil, fmt.Errorf("modprobe zram: %w", err)
	}
	id, err := ops.hotAdd()
	if err != nil {
		return nil, fmt.Errorf("zram hot_add: %w", err)
	}
	// Crash-during-setup cleanup (design-review Med): if any step below errors,
	// remove the just-added device rather than leaking a half-set-up zram.
	setupOK := false
	defer func() {
		if !setupOK {
			_ = ops.hotRemove(id)
		}
	}()

	memLimit := ramBytes * int64(p.MaxRAMPercent) / 100 // multiply-FIRST (design-review High)
	if memLimit <= 0 {                                  // kernel treats 0 as UNLIMITED (design-review Low)
		return nil, fmt.Errorf("zram mem_limit computed <= 0 (ram=%d pct=%d)", ramBytes, p.MaxRAMPercent)
	}
	disksize := 3 * memLimit

	if err := ops.writeAttr(id, "comp_algorithm", p.Algorithm); err != nil {
		return nil, fmt.Errorf("zram comp_algorithm: %w", err)
	}
	// Exact bracketed-token match, not substring (design-review Low): "lzo" must
	// not match an active "[lzo-rle]".
	if got, _ := ops.readAttr(id, "comp_algorithm"); activeAlgorithm(got) != p.Algorithm {
		return nil, fmt.Errorf("zram rejected algorithm %q (read back %q)", p.Algorithm, got)
	}
	if err := ops.writeAttr(id, "disksize", strconv.FormatInt(disksize, 10)); err != nil {
		return nil, fmt.Errorf("zram disksize: %w", err)
	}
	if err := ops.writeAttr(id, "mem_limit", strconv.FormatInt(memLimit, 10)); err != nil {
		return nil, fmt.Errorf("zram mem_limit: %w", err)
	}
	if err := ops.mkswap(id, swapLabel); err != nil { // -L otxzram stamps ownership (design-review Blocker)
		return nil, fmt.Errorf("zram mkswap: %w", err)
	}
	if err := ops.swapon(id, swapPriority); err != nil {
		return nil, fmt.Errorf("zram swapon: %w", err)
	}
	setupOK = true // past the last fallible op; the deferred cleanup will not fire
	return &Active{
		Device:      fmt.Sprintf("/dev/zram%d", id),
		Kind:        "zram",
		SizeMib:     disksize / (1024 * 1024),
		MemLimitMib: memLimit / (1024 * 1024),
		Algorithm:   p.Algorithm,
	}, nil
}
