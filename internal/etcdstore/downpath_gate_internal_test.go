// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDownPathReachable(t *testing.T) {
	gw := uuid.New()
	other := uuid.New()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	staleness := 90 * time.Second
	fresh := now.Add(-30 * time.Second)
	publicGateways := map[uuid.UUID]bool{gw: true}

	tests := []struct {
		name        string
		natd        bool
		established []string
		reportedAt  time.Time
		gateways    map[uuid.UUID]bool
		wantReach   bool
	}{
		{name: "public node short-circuits reachable", natd: false, established: nil, reportedAt: time.Time{}, gateways: nil, wantReach: true},
		{name: "natd lists a public gateway reachable", natd: true, established: []string{gw.String()}, reportedAt: fresh, gateways: publicGateways, wantReach: true},
		{name: "natd lists only a non-gateway unreachable", natd: true, established: []string{other.String()}, reportedAt: fresh, gateways: publicGateways, wantReach: false},
		{name: "natd lists no peers unreachable", natd: true, established: nil, reportedAt: fresh, gateways: publicGateways, wantReach: false},
		{name: "natd at staleness boundary reachable", natd: true, established: []string{gw.String()}, reportedAt: now.Add(-staleness), gateways: publicGateways, wantReach: true},
		{name: "natd past staleness unreachable", natd: true, established: []string{gw.String()}, reportedAt: now.Add(-staleness - time.Nanosecond), gateways: publicGateways, wantReach: false},
		{name: "unparseable peer skipped, valid gateway still matches", natd: true, established: []string{"not-a-uuid", gw.String()}, reportedAt: fresh, gateways: publicGateways, wantReach: true},
		{name: "only an unparseable peer unreachable", natd: true, established: []string{"not-a-uuid"}, reportedAt: fresh, gateways: publicGateways, wantReach: false},
	}
	for _, tt := range tests {
		got := downPathReachable(tt.natd, tt.established, tt.reportedAt, tt.gateways, now, staleness)
		if got != tt.wantReach {
			t.Errorf("%s: downPathReachable(natd=%v) = %v, want %v", tt.name, tt.natd, got, tt.wantReach)
		}
	}
}
