// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package zram

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Ensure reconciles the node to the desired zram state described by p, reading
// host RAM and current zram state itself. It is idempotent across agent
// restarts. The caller must treat a returned error as non-fatal (log WARN and
// continue) so a setup failure never crashes the agent.
func Ensure(p Params, log *slog.Logger) (*Active, error) {
	if p.Enabled && p.Algorithm == "" {
		p.Algorithm = "zstd"
	}
	ram, err := readMemTotalBytes("/proc")
	if err != nil {
		return nil, fmt.Errorf("read host RAM: %w", err)
	}
	return ensureWith(p, realHost{}, ram, Observe())
}

func readMemTotalBytes(procRoot string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo")) // #nosec G304 -- fixed procfs path
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kib * 1024, nil
	}
	return 0, fmt.Errorf("MemTotal not found in meminfo")
}

// realHost implements hostOps against the live kernel.
type realHost struct{}

func (realHost) modprobe() error {
	// The module may be builtin; ignore an already-loaded / builtin result.
	if out, err := exec.Command("modprobe", "zram").CombinedOutput(); err != nil {
		return fmt.Errorf("modprobe zram: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (realHost) hotAdd() (int, error) {
	b, err := os.ReadFile("/sys/class/zram-control/hot_add")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func (realHost) hotRemove(id int) error {
	return os.WriteFile("/sys/class/zram-control/hot_remove", []byte(strconv.Itoa(id)), 0o644) // #nosec G306
}

func (realHost) writeAttr(dev int, attr, val string) error {
	p := fmt.Sprintf("/sys/block/zram%d/%s", dev, attr)
	return os.WriteFile(p, []byte(val), 0o644) // #nosec G306,G304 -- fixed sysfs attr path
}

func (realHost) readAttr(dev int, attr string) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/sys/block/zram%d/%s", dev, attr)) // #nosec G304
	return string(b), err
}

func (realHost) mkswap(dev int, label string) error {
	if out, err := exec.Command("mkswap", "-L", label, fmt.Sprintf("/dev/zram%d", dev)).CombinedOutput(); err != nil {
		return fmt.Errorf("mkswap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (realHost) swapon(dev, prio int) error {
	if out, err := exec.Command("swapon", "-p", strconv.Itoa(prio), fmt.Sprintf("/dev/zram%d", dev)).CombinedOutput(); err != nil {
		return fmt.Errorf("swapon: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (realHost) swapoff(dev int) error {
	if out, err := exec.Command("swapoff", fmt.Sprintf("/dev/zram%d", dev)).CombinedOutput(); err != nil {
		return fmt.Errorf("swapoff: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
