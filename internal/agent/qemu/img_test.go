// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import "testing"

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
