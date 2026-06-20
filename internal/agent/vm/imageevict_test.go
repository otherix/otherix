// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"strings"
	"testing"
)

func TestTryLockImageBlobSkipsHeldDigest(t *testing.T) {
	m := &Manager{} // bare manager; lockForImage uses the zero-value sync.Map
	digest := strings.Repeat("a", 64)
	rel1, ok1 := m.TryLockImageBlob(digest)
	if !ok1 {
		t.Fatalf("first TryLockImageBlob = false, want true")
	}
	if _, ok2 := m.TryLockImageBlob(digest); ok2 {
		t.Errorf("second TryLockImageBlob = true while held, want false (eviction must skip a mid-clone digest)")
	}
	rel1()
	rel3, ok3 := m.TryLockImageBlob(digest)
	if !ok3 {
		t.Errorf("TryLockImageBlob after release = false, want true")
	}
	rel3()
}

func TestSetImageEvictionNudgeInvoked(t *testing.T) {
	m := &Manager{}
	got := 0
	m.SetImageEvictionNudge(func() { got++ })
	m.imageEvictionNudge()
	if got != 1 {
		t.Errorf("nudge invoked %d times, want 1", got)
	}
}
