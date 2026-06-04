// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"testing"
	"time"
)

func TestPeerEstablished(t *testing.T) {
	now := time.Unix(1000, 0)
	fresh := now.Add(-10 * time.Second)
	stale := now.Add(-2 * wgStaleWindow)
	tests := []struct {
		name              string
		rawEstablished    bool
		selfLastHeartbeat *time.Time
		want              bool
	}{
		{name: "fresh", rawEstablished: true, selfLastHeartbeat: &fresh, want: true},
		{name: "stale", rawEstablished: true, selfLastHeartbeat: &stale, want: false},
		{name: "nil heartbeat", rawEstablished: true, selfLastHeartbeat: nil, want: false},
		{name: "raw false", rawEstablished: false, selfLastHeartbeat: &fresh, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerEstablished(tt.rawEstablished, tt.selfLastHeartbeat, now); got != tt.want {
				t.Errorf("peerEstablished(%v, %v, now) = %v, want %v",
					tt.rawEstablished, tt.selfLastHeartbeat, got, tt.want)
			}
		})
	}
}
