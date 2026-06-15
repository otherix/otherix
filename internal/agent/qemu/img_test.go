// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseImgInfo(t *testing.T) {
	const out = `{"virtual-size": 3758096384, "filename": "x.qcow2", "format": "qcow2", "actual-size": 565182464}`
	info, err := parseImgInfo([]byte(out))
	if err != nil {
		t.Fatalf("parseImgInfo() error = %v", err)
	}
	if info.VirtualSize != 3758096384 {
		t.Errorf("VirtualSize = %d, want 3758096384", info.VirtualSize)
	}
	if info.Format != "qcow2" {
		t.Errorf("Format = %q, want qcow2", info.Format)
	}
}

func TestParseImgInfoRejectsGarbage(t *testing.T) {
	if _, err := parseImgInfo([]byte("not json")); err == nil {
		t.Errorf("parseImgInfo(garbage) error = nil, want non-nil")
	}
}

func TestImgVirtualSizeShared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", path, "64M").CombinedOutput(); err != nil {
		t.Skipf("qemu-img not available: %v (%s)", err, out)
	}
	got, err := ImgVirtualSizeShared(context.Background(), path)
	if err != nil {
		t.Fatalf("ImgVirtualSizeShared: %v", err)
	}
	if got != 64*1024*1024 {
		t.Errorf("virtual size = %d, want %d", got, 64*1024*1024)
	}
}
