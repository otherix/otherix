// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package agent

import "testing"

func TestFreeBytesStatfsReturnsPositive(t *testing.T) {
	n, err := freeBytesStatfs(t.TempDir())
	if err != nil {
		t.Fatalf("freeBytesStatfs: %v", err)
	}
	if n == 0 {
		t.Errorf("freeBytesStatfs = 0, want > 0 for a writable tmpdir")
	}
}
