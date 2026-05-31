// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"testing"

	"github.com/otherix/otherix/internal/api"
)

func TestBuildInitialCluster(t *testing.T) {
	cases := []struct {
		name        string
		members     []api.ClusterMemberRef
		selfName    string
		selfPeerURL string
		want        string
	}{
		{
			// First join: the just-registered learner (self) is echoed back with
			// an empty etcd name. The trailing add() keys it by selfName, and the
			// peer-URL-keyed order map lists self exactly once.
			name: "first join keys empty-name self by configured name",
			members: []api.ClusterMemberRef{
				{Name: "n0", PeerURL: "https://10.0.0.1:2380"},
				{Name: "", PeerURL: "https://10.0.0.2:2380"},
			},
			selfName:    "n2",
			selfPeerURL: "https://10.0.0.2:2380",
			want:        "n0=https://10.0.0.1:2380,n2=https://10.0.0.2:2380",
		},
		{
			name: "member with empty peer url is skipped",
			members: []api.ClusterMemberRef{
				{Name: "n0", PeerURL: "https://10.0.0.1:2380"},
				{Name: "n1", PeerURL: ""},
			},
			selfName:    "n2",
			selfPeerURL: "https://10.0.0.2:2380",
			want:        "n0=https://10.0.0.1:2380,n2=https://10.0.0.2:2380",
		},
		{
			name: "self absent from members is still appended",
			members: []api.ClusterMemberRef{
				{Name: "n0", PeerURL: "https://10.0.0.1:2380"},
			},
			selfName:    "n1",
			selfPeerURL: "https://10.0.0.2:2380",
			want:        "n0=https://10.0.0.1:2380,n1=https://10.0.0.2:2380",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildInitialCluster(tc.members, tc.selfName, tc.selfPeerURL)
			if got != tc.want {
				t.Errorf("buildInitialCluster(%v, %q, %q) = %q, want %q",
					tc.members, tc.selfName, tc.selfPeerURL, got, tc.want)
			}
		})
	}
}
