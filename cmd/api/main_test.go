// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/etcdstore"
)

// TestWarnIfUnderlayMTUBelowFloor is the fail-toward-visibility guarantee for a
// legacy sub-1390 underlay seed: UnderlayMTU returns the immutable seed verbatim
// (clamping it up would push the derived overlay MTU too high and fragment), so
// the boot path must WARN - naming the seed, the derived overlay MTU, and the
// floor - rather than correct it. The default 1500, the floor itself, and any
// valid seed must NOT warn.
func TestWarnIfUnderlayMTUBelowFloor(t *testing.T) {
	cases := []struct {
		name     string
		mtu      int32
		wantWarn bool
	}{
		{name: "legacy 1280 seed warns", mtu: 1280, wantWarn: true},
		{name: "one below the floor warns", mtu: etcdstore.MinUnderlayMTU - 1, wantWarn: true},
		{name: "exactly the floor is silent", mtu: etcdstore.MinUnderlayMTU, wantWarn: false},
		{name: "default 1500 is silent", mtu: 1500, wantWarn: false},
		{name: "jumbo 9000 is silent", mtu: 9000, wantWarn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			got := warnIfUnderlayMTUBelowFloor(tc.mtu, log)
			if got != tc.wantWarn {
				t.Errorf("warnIfUnderlayMTUBelowFloor(%d) = %v, want %v", tc.mtu, got, tc.wantWarn)
			}
			loggedWarn := strings.Contains(buf.String(), "level=WARN")
			if loggedWarn != tc.wantWarn {
				t.Errorf("WARN logged = %v, want %v (log: %q)", loggedWarn, tc.wantWarn, buf.String())
			}
			if tc.wantWarn {
				// The WARN must name the seed and the derived overlay MTU so the
				// operator can act without reading source.
				out := buf.String()
				for _, sub := range []string{"underlay_mtu", "overlay_mtu", "floor"} {
					if !strings.Contains(out, sub) {
						t.Errorf("WARN missing %q: %q", sub, out)
					}
				}
			}
		})
	}
}

func TestRootCmdHasServeAndJoin(t *testing.T) {
	root := newRootCmd()
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name())
	}
	wantServe, wantJoin := false, false
	for _, n := range names {
		if n == "serve" {
			wantServe = true
		}
		if n == "join" {
			wantJoin = true
		}
	}
	if !wantServe || !wantJoin {
		t.Errorf("root subcommands = %v, want both serve and join", names)
	}
	if root.RunE == nil {
		t.Errorf("bare otherix-api must default to serve (root.RunE is nil)")
	}
}

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
		{
			// Self's configured peer_url carries a trailing slash while etcd echoes
			// the bare form. Canonicalizing both collapses them to one key, so self
			// appears exactly once under the configured name (no duplicate).
			name: "trailing-slash self dedups against echoed bare form",
			members: []api.ClusterMemberRef{
				{Name: "n0", PeerURL: "https://10.0.0.1:2380"},
				{Name: "", PeerURL: "https://10.0.0.2:2380"},
			},
			selfName:    "n2",
			selfPeerURL: "https://10.0.0.2:2380/",
			want:        "n0=https://10.0.0.1:2380,n2=https://10.0.0.2:2380",
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
