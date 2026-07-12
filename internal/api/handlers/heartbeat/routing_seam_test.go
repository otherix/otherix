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
	defer os.RemoveAll(dir)

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
// producer over a real store: two NAT'd agents and one gateway, with a mutual
// handshake between each NAT'd agent and the gateway (but none between the two
// NAT'd agents). natA's declared peer set must be exactly the gateway (direct),
// carrying its own /32 plus natB's relayed /32; natB gets no direct entry.
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

	overlayB := netip.PrefixFrom(mustOverlayIP(t, s, natB), 32).String()
	overlayGW := netip.PrefixFrom(mustOverlayIP(t, s, gw), 32).String()

	h := &Handler{log: discardLogger()}
	var got []declaredWireGuardPeer
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		var e error
		got, e = h.loadDeclaredWireGuardPeers(ctx, hp, natA)
		return e
	}); err != nil {
		t.Fatalf("run projection: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want a single (gateway) peer entry, got %d: %+v", len(got), got)
	}
	e := findPeer(got, "pk-gw")
	if e == nil {
		t.Fatalf("gateway entry missing: %+v", got)
	}
	if e.Endpoint != "9.9.9.9:51820" {
		t.Errorf("gateway endpoint = %q, want 9.9.9.9:51820", e.Endpoint)
	}
	if !hasAllowed(e, overlayGW) {
		t.Errorf("gateway entry missing its own /32 %s: %+v", overlayGW, e)
	}
	if !hasAllowed(e, overlayB) {
		t.Errorf("gateway entry missing relayed peer /32 %s: %+v", overlayB, e)
	}
	if findPeer(got, "pk-b") != nil {
		t.Errorf("relayed NAT'd peer must not get a direct entry: %+v", got)
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
