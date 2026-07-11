// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package zram

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseZramSwapDevices(t *testing.T) {
	// /proc/swaps: a header line, then one row per swap. Column 0 is Filename.
	in := "Filename\t\t\t\tType\t\tSize\tUsed\tPriority\n" +
		"/dev/zram0                              partition\t805300\t0\t100\n" +
		"/dev/nvme0n1p3                          partition\t2097148\t0\t-2\n" +
		"/dev/zram1                              partition\t402650\t0\t100\n"
	got := parseZramSwapDevices(in)
	want := []string{"/dev/zram0", "/dev/zram1"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("parseZramSwapDevices mismatch (-want +got):\n%s", diff)
	}
}

func TestParseZramSwapDevicesNone(t *testing.T) {
	in := "Filename\t\t\t\tType\t\tSize\tUsed\tPriority\n" +
		"/dev/nvme0n1p3                          partition\t2097148\t0\t-2\n"
	if got := parseZramSwapDevices(in); len(got) != 0 {
		t.Errorf("parseZramSwapDevices = %v, want empty", got)
	}
}

func TestObserveLargestPicksBiggestDisksize(t *testing.T) {
	sysRoot := t.TempDir()
	// zram0 small, zram1 large -> Observe must report zram1 (the honest net).
	writeFile(t, filepath.Join(sysRoot, "block", "zram0", "disksize"), "402653184\n") // 384 MiB
	writeFile(t, filepath.Join(sysRoot, "block", "zram0", "mem_limit"), "0\n")
	writeFile(t, filepath.Join(sysRoot, "block", "zram0", "comp_algorithm"), "lzo [zstd] lz4\n")
	writeFile(t, filepath.Join(sysRoot, "block", "zram1", "disksize"), "805306368\n") // 768 MiB
	writeFile(t, filepath.Join(sysRoot, "block", "zram1", "mem_limit"), "0\n")
	writeFile(t, filepath.Join(sysRoot, "block", "zram1", "comp_algorithm"), "lzo [zstd] lz4\n")
	// mm_stat: orig_data_size (field 0) = 240 MiB swapped-in, mem_used_total
	// (field 2) = 60 MiB physical RAM footprint. Only the winning device's line
	// matters.
	writeFile(t, filepath.Join(sysRoot, "block", "zram1", "mm_stat"), "251658240 60000000 62914560 0 62914560 0 0 0\n")

	got := observeLargest([]string{"/dev/zram0", "/dev/zram1"}, sysRoot)
	want := &Active{Device: "/dev/zram1", Kind: "zram", SizeMib: 768, MemLimitMib: 0, Algorithm: "zstd", SwappedMib: 240, RAMUsedMib: 60}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("observeLargest mismatch (-want +got):\n%s", diff)
	}
}

func TestParseMemStat(t *testing.T) {
	orig, memUsed, ok := parseMemStat("251658240 60000000 62914560 0 62914560 0 0 0")
	if !ok || orig != 251658240 || memUsed != 62914560 {
		t.Errorf("parseMemStat = (%d, %d, %v), want (251658240, 62914560, true)", orig, memUsed, ok)
	}
	if _, _, ok := parseMemStat("123 456"); ok {
		t.Errorf("parseMemStat(short line) ok = true, want false")
	}
	if _, _, ok := parseMemStat("x 456 789"); ok {
		t.Errorf("parseMemStat(non-numeric) ok = true, want false")
	}
}

func TestObserveLargestNoneIsNil(t *testing.T) {
	if got := observeLargest(nil, t.TempDir()); got != nil {
		t.Errorf("observeLargest(nil) = %+v, want nil", got)
	}
}

func TestActiveAlgorithmBracket(t *testing.T) {
	if got := activeAlgorithm("lzo [zstd] lz4"); got != "zstd" {
		t.Errorf("activeAlgorithm = %q, want zstd", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
