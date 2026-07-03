// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

func TestSelectBackendEmpty(t *testing.T) {
	got, ok := selectBackend(nil, func(int) int { t.Fatalf("rnd must not be called on an empty set"); return 0 })
	if ok {
		t.Errorf("selectBackend(nil) ok = true, want false")
	}
	if got != (heartbeat.DeclaredBackend{}) {
		t.Errorf("selectBackend(nil) backend = %+v, want zero value", got)
	}
}

func TestSelectBackendPicksIndex(t *testing.T) {
	backends := []heartbeat.DeclaredBackend{
		{OverlayIP: "10.0.0.1"},
		{OverlayIP: "10.0.0.2"},
		{OverlayIP: "10.0.0.3"},
	}
	got, ok := selectBackend(backends, func(int) int { return 1 })
	if !ok {
		t.Fatalf("selectBackend(3 backends) ok = false, want true")
	}
	if got != backends[1] {
		t.Errorf("selectBackend(3 backends, rnd->1) = %+v, want %+v", got, backends[1])
	}
}

func TestSelectBackendBalances(t *testing.T) {
	backends := []heartbeat.DeclaredBackend{
		{VMID: uuid.New(), OverlayIP: "10.0.0.1"},
		{VMID: uuid.New(), OverlayIP: "10.0.0.2"},
		{VMID: uuid.New(), OverlayIP: "10.0.0.3"},
	}
	seen := make(map[uuid.UUID]bool, len(backends))
	for range 1000 {
		got, ok := selectBackend(backends, rand.IntN)
		if !ok {
			t.Fatalf("selectBackend ok = false, want true")
		}
		seen[got.VMID] = true
	}
	for _, b := range backends {
		if !seen[b.VMID] {
			t.Errorf("backend %v never selected over 1000 calls", b.VMID)
		}
	}
}
