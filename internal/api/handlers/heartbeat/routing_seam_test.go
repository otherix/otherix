// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package heartbeat

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// sharedClient is one embedded etcd member reused across this test binary, with a
// per-test keyspace wipe for isolation - mirrors internal/etcdstore/main_test.go.
var sharedClient *etcd.Client

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "otherix-heartbeat")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	// os.Exit skips deferred cleanup, so remove the temp dir explicitly below.

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	rt, err := etcd.Start(ctx, &etcd.Config{
		Mode:          etcd.ModeSingle,
		Name:          "heartbeat-test",
		DataDir:       filepath.Join(dir, "member"),
		PeerURL:       fmt.Sprintf("http://127.0.0.1:%d", freeTestPort()),
		ClientURL:     fmt.Sprintf("http://127.0.0.1:%d", freeTestPort()),
		ClusterToken:  "otherix-heartbeat-test",
		UnsafeNoFsync: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcd.Start: %v\n", err)
		os.Exit(1)
	}
	sharedClient = etcd.NewClient(rt)

	code := m.Run()

	_ = sharedClient.Close()
	rt.Stop(10 * time.Second)
	os.RemoveAll(dir)
	os.Exit(code)
}

// freshStore wipes the shared member's keyspace and returns a Store over it.
func freshStore(tb testing.TB) *etcdstore.Store {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := sharedClient.Raw().Delete(ctx, etcd.KeyPrefix, clientv3.WithPrefix()); err != nil {
		tb.Fatalf("wipe keyspace: %v", err)
	}
	return etcdstore.New(sharedClient)
}

func freeTestPort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("freeTestPort: %v", err))
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestLoadDeclaredWireGuardPeers_RelaysNATdPairThroughGateway drives the real
// producer over a real store for all three nodes of a relay triangle: two NAT'd
// agents and one gateway, with a mutual handshake between each NAT'd agent and
// the gateway (but none between the two NAT'd agents). It asserts the full
// A<->G<->B forwarding path:
//   - natA's declared set is exactly the gateway (direct), carrying its own /32
//     plus natB's relayed /32; natB gets no direct entry;
//   - natB is symmetric: gateway direct, own /32 plus natA's relayed /32;
//   - the gateway forwards to each dialer - TWO endpoint-less /32 entries, one per
//     NAT'd peer, neither overwriting the other (the relay's own half).
func TestLoadDeclaredWireGuardPeers_RelaysNATdPairThroughGateway(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()

	natA, natB, gw := uuid.New(), uuid.New(), uuid.New()
	mustCreateNode(t, s, natA, "nat-a", false)
	mustCreateNode(t, s, natB, "nat-b", false)
	mustCreateNode(t, s, gw, "gw", true)

	mustUpsertWG(t, s, natA, "pk-a", "", []string{gw.String()})
	mustUpsertWG(t, s, natB, "pk-b", "", []string{gw.String()})
	mustUpsertWG(t, s, gw, "pk-gw", "9.9.9.9:51820", []string{natA.String(), natB.String()})

	overlayA := netip.PrefixFrom(mustOverlayIP(t, s, natA), 32).String()
	overlayB := netip.PrefixFrom(mustOverlayIP(t, s, natB), 32).String()
	overlayGW := netip.PrefixFrom(mustOverlayIP(t, s, gw), 32).String()

	h := &Handler{log: discardLogger()}
	var gotA, gotB, gotGW []declaredWireGuardPeer
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		var e error
		if gotA, e = h.loadDeclaredWireGuardPeers(ctx, hp, natA); e != nil {
			return e
		}
		if gotB, e = h.loadDeclaredWireGuardPeers(ctx, hp, natB); e != nil {
			return e
		}
		gotGW, e = h.loadDeclaredWireGuardPeers(ctx, hp, gw)
		return e
	}); err != nil {
		t.Fatalf("run projection: %v", err)
	}

	// natA relays to natB through the gateway.
	if len(gotA) != 1 {
		t.Fatalf("natA: want a single (gateway) peer entry, got %d: %+v", len(gotA), gotA)
	}
	if e := findPeer(gotA, "pk-gw"); e == nil {
		t.Fatalf("natA: gateway entry missing: %+v", gotA)
	} else {
		if e.Endpoint != "9.9.9.9:51820" {
			t.Errorf("natA: gateway endpoint = %q, want 9.9.9.9:51820", e.Endpoint)
		}
		if !hasAllowed(e, overlayGW) {
			t.Errorf("natA: gateway entry missing its own /32 %s: %+v", overlayGW, e)
		}
		if !hasAllowed(e, overlayB) {
			t.Errorf("natA: gateway entry missing relayed peer /32 %s: %+v", overlayB, e)
		}
	}
	if findPeer(gotA, "pk-b") != nil {
		t.Errorf("natA: relayed NAT'd peer must not get a direct entry: %+v", gotA)
	}

	// natB is symmetric: it relays to natA through the same gateway.
	if len(gotB) != 1 {
		t.Fatalf("natB: want a single (gateway) peer entry, got %d: %+v", len(gotB), gotB)
	}
	if e := findPeer(gotB, "pk-gw"); e == nil {
		t.Fatalf("natB: gateway entry missing: %+v", gotB)
	} else if !hasAllowed(e, overlayGW) || !hasAllowed(e, overlayA) {
		t.Errorf("natB: gateway entry must carry own %s and relayed %s: %+v", overlayGW, overlayA, e)
	}
	if findPeer(gotB, "pk-a") != nil {
		t.Errorf("natB: relayed NAT'd peer must not get a direct entry: %+v", gotB)
	}

	// The gateway forwards to each dialer: two endpoint-less /32 entries, one per
	// NAT'd peer, neither overwriting the other.
	if len(gotGW) != 2 {
		t.Fatalf("gw: want two forward entries (one per dialer), got %d: %+v", len(gotGW), gotGW)
	}
	for _, want := range []struct{ pubkey, overlay string }{{"pk-a", overlayA}, {"pk-b", overlayB}} {
		e := findPeer(gotGW, want.pubkey)
		if e == nil {
			t.Errorf("gw: missing forward entry for dialer %s: %+v", want.pubkey, gotGW)
			continue
		}
		if e.Endpoint != "" {
			t.Errorf("gw: forward entry %s must be endpoint-less, got %q", want.pubkey, e.Endpoint)
		}
		if !hasAllowed(e, want.overlay) {
			t.Errorf("gw: forward entry %s must carry the dialer /32 %s: %+v", want.pubkey, want.overlay, e)
		}
	}
}

func mustCreateNode(t *testing.T, s *etcdstore.Store, id uuid.UUID, name string, gateway bool) {
	t.Helper()
	if _, err := s.CreateNode(context.Background(), store.CreateNodeParams{
		ID:           id,
		Name:         name,
		Gateway:      gateway,
		Architecture: store.CpuArchAmd64,
		Status:       store.NodeStatusReady,
	}); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
}

func mustUpsertWG(t *testing.T, s *etcdstore.Store, id uuid.UUID, pubkey, endpoint string, established []string) {
	t.Helper()
	if err := s.UpsertAgentWireguard(context.Background(), store.UpsertAgentWireguardParams{
		NodeID:           id,
		PublicKey:        pubkey,
		Endpoint:         endpoint,
		EstablishedPeers: established,
	}); err != nil {
		t.Fatalf("upsert wg %s: %v", pubkey, err)
	}
}

func mustOverlayIP(t *testing.T, s *etcdstore.Store, id uuid.UUID) netip.Addr {
	t.Helper()
	rec, err := s.AgentWireguardByNodeID(context.Background(), id)
	if err != nil {
		t.Fatalf("read wg %s: %v", id, err)
	}
	return rec.OverlayIP
}
