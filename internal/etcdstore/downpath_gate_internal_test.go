// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDownPathReachable(t *testing.T) {
	node := uuid.New()
	other := uuid.New()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	staleness := 90 * time.Second

	tests := []struct {
		name      string
		natd      bool
		freshest  map[uuid.UUID]time.Time
		wantReach bool
	}{
		{name: "public node always reachable", natd: false, freshest: nil, wantReach: true},
		{name: "public node reachable even with no gateway entry", natd: false, freshest: map[uuid.UUID]time.Time{other: now}, wantReach: true},
		{name: "natd node with no gateway entry unreachable", natd: true, freshest: map[uuid.UUID]time.Time{other: now}, wantReach: false},
		{name: "natd node with fresh handshake reachable", natd: true, freshest: map[uuid.UUID]time.Time{node: now.Add(-30 * time.Second)}, wantReach: true},
		{name: "natd node at staleness boundary reachable", natd: true, freshest: map[uuid.UUID]time.Time{node: now.Add(-staleness)}, wantReach: true},
		{name: "natd node past staleness unreachable", natd: true, freshest: map[uuid.UUID]time.Time{node: now.Add(-staleness - time.Nanosecond)}, wantReach: false},
	}
	for _, tt := range tests {
		got := downPathReachable(tt.natd, node, tt.freshest, now, staleness)
		if got != tt.wantReach {
			t.Errorf("%s: downPathReachable(natd=%v) = %v, want %v", tt.name, tt.natd, got, tt.wantReach)
		}
	}
}
