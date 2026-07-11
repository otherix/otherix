// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package zram

import (
	"strings"
	"testing"
)

// fakeHost records the ops Ensure issues and simulates hot_add returning a
// device id.
type fakeHost struct {
	calls      []string
	nextDev    int
	removedDev int
}

func (f *fakeHost) modprobe() error { f.calls = append(f.calls, "modprobe"); return nil }
func (f *fakeHost) hotAdd() (int, error) {
	f.calls = append(f.calls, "hotAdd")
	return f.nextDev, nil
}

func (f *fakeHost) hotRemove(id int) error {
	f.removedDev = id
	f.calls = append(f.calls, "hotRemove")
	return nil
}

func (f *fakeHost) writeAttr(dev int, attr, val string) error {
	f.calls = append(f.calls, "write:"+attr+"="+val)
	return nil
}
func (f *fakeHost) readAttr(dev int, attr string) (string, error) { return "[zstd]", nil }
func (f *fakeHost) mkswap(dev int, label string) error {
	f.calls = append(f.calls, "mkswap:"+label)
	return nil
}

func (f *fakeHost) swapon(dev, prio int) error {
	f.calls = append(f.calls, "swapon")
	return nil
}
func (f *fakeHost) swapoff(dev int) error { f.calls = append(f.calls, "swapoff"); return nil }

func TestEnsurePresentSetsUpFreshDevice(t *testing.T) {
	f := &fakeHost{nextDev: 3}
	ramBytes := int64(1024) * 1024 * 1024 // 1 GiB
	got, err := ensureWith(Params{Enabled: true, MaxRAMPercent: 25, Algorithm: "zstd"}, f, ramBytes, nil)
	if err != nil {
		t.Fatalf("ensureWith: %v", err)
	}
	// mem_limit = 25% of 1 GiB = 256 MiB; disksize = 3x = 768 MiB.
	if got == nil || got.SizeMib != 768 || got.MemLimitMib != 256 || got.Device != "/dev/zram3" {
		t.Fatalf("Active = %+v, want size 768 memlimit 256 dev /dev/zram3", got)
	}
	seq := strings.Join(f.calls, ",")
	// order: hot_add, comp_algorithm, disksize, mem_limit, mkswap -L otxzram, swapon.
	// mem_limit = 1073741824 * 25 / 100 = 268435456 (multiply-first); disksize = 3x.
	want := "hotAdd,write:comp_algorithm=zstd,write:disksize=805306368,write:mem_limit=268435456,mkswap:otxzram,swapon"
	if !strings.Contains(seq, want) {
		t.Errorf("op sequence = %q, want to contain %q", seq, want)
	}
}

func TestEnsurePresentIdempotentWhenAlreadyActive(t *testing.T) {
	f := &fakeHost{nextDev: 9}
	existing := &Active{Device: "/dev/zram3", Kind: "zram", SizeMib: 768, MemLimitMib: 256, Algorithm: "zstd"}
	got, err := ensureWith(Params{Enabled: true, MaxRAMPercent: 25, Algorithm: "zstd"}, f, int64(1024)*1024*1024, existing)
	if err != nil {
		t.Fatalf("ensureWith: %v", err)
	}
	if got != existing {
		t.Errorf("expected the existing device returned unchanged, got %+v", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("expected no host ops on idempotent re-run, got %v", f.calls)
	}
}

func TestEnsureDisabledTearsDownExisting(t *testing.T) {
	f := &fakeHost{}
	existing := &Active{Device: "/dev/zram3", Kind: "zram"}
	got, err := ensureWith(Params{Enabled: false}, f, 0, existing)
	if err != nil {
		t.Fatalf("ensureWith: %v", err)
	}
	if got != nil {
		t.Errorf("disabled Ensure = %+v, want nil", got)
	}
	if f.removedDev != 3 || strings.Join(f.calls, ",") != "swapoff,hotRemove" {
		t.Errorf("teardown ops = %v (removed %d), want swapoff,hotRemove on dev 3", f.calls, f.removedDev)
	}
}

func TestEnsureDisabledNoExistingIsNoop(t *testing.T) {
	f := &fakeHost{}
	got, err := ensureWith(Params{Enabled: false}, f, 0, nil)
	if err != nil || got != nil {
		t.Fatalf("disabled+no-existing = (%+v, %v), want (nil, nil)", got, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("expected no ops, got %v", f.calls)
	}
}
