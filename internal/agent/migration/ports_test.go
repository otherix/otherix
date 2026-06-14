// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration

import (
	"errors"
	"testing"
)

func TestPortAllocatorReserveRelease(t *testing.T) {
	a := NewPortAllocator(49152, 49153) // two ports

	p1, err := a.Reserve()
	if err != nil {
		t.Fatalf("Reserve() #1 error = %v", err)
	}
	p2, err := a.Reserve()
	if err != nil {
		t.Fatalf("Reserve() #2 error = %v", err)
	}
	if p1 == p2 {
		t.Errorf("Reserve() returned duplicate port %d", p1)
	}
	if p1 < 49152 || p1 > 49153 || p2 < 49152 || p2 > 49153 {
		t.Errorf("Reserve() ports %d,%d outside [49152,49153]", p1, p2)
	}

	// Exhausted.
	if _, err := a.Reserve(); !errors.Is(err, ErrNoFreePort) {
		t.Errorf("Reserve() on exhausted pool error = %v, want ErrNoFreePort", err)
	}

	// Release one, reserve again succeeds and reuses it.
	a.Release(p1)
	p3, err := a.Reserve()
	if err != nil {
		t.Fatalf("Reserve() after Release error = %v", err)
	}
	if p3 != p1 {
		t.Errorf("Reserve() after Release(%d) = %d, want reuse %d", p1, p3, p1)
	}
}

func TestPortAllocatorReleaseUnknownIsNoop(t *testing.T) {
	a := NewPortAllocator(49152, 49152)
	a.Release(40000) // out of range, must not panic or corrupt state
	p, err := a.Reserve()
	if err != nil || p != 49152 {
		t.Errorf("Reserve() = %d,%v, want 49152,nil", p, err)
	}
}
