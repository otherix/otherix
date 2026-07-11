// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package zram

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseSwaponLabels(t *testing.T) {
	// `swapon --show=NAME,LABEL --noheadings --raw` output: NAME then LABEL, a
	// blank LABEL when unset.
	in := "/dev/zram3 otxzram\n/dev/zram0\n/dev/nvme0n1p3 myswap\n"
	got := parseSwaponLabels(in)
	want := map[string]string{"/dev/zram3": "otxzram", "/dev/zram0": "", "/dev/nvme0n1p3": "myswap"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("parseSwaponLabels mismatch (-want +got):\n%s", diff)
	}
}

func TestObserveOwnedReadsLabelledZram(t *testing.T) {
	sysRoot := t.TempDir()
	dev := filepath.Join(sysRoot, "block", "zram3")
	writeFile(t, filepath.Join(dev, "disksize"), "805306368\n")            // 768 MiB
	writeFile(t, filepath.Join(dev, "mem_limit"), "268435456\n")           // 256 MiB
	writeFile(t, filepath.Join(dev, "comp_algorithm"), "lzo [zstd] lz4\n") // bracket = active

	// A distro zram0 (different label) coexists with OUR labelled zram3.
	labels := map[string]string{"/dev/zram0": "distro", "/dev/zram3": "otxzram"}
	got := observeOwned(labels, sysRoot, "otxzram")
	want := &Active{Device: "/dev/zram3", Kind: "zram", SizeMib: 768, MemLimitMib: 256, Algorithm: "zstd"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("observeOwned mismatch (-want +got):\n%s", diff)
	}
}

func TestObserveOwnedIgnoresNonOtherixZram(t *testing.T) {
	// design-review Blocker: a distro zram (no otxzram label) must NOT be observed
	// as ours, so default-off never sees or tears down a distro's swap.
	labels := map[string]string{"/dev/zram0": "distro", "/dev/nvme0n1p3": "myswap"}
	if got := observeOwned(labels, t.TempDir(), "otxzram"); got != nil {
		t.Errorf("observeOwned with no otxzram label = %+v, want nil", got)
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
