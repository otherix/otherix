// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/config"
)

func mustKey(t *testing.T) wgtypes.Key {
	t.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

func TestNewWireGuard_RejectsNilFabric(t *testing.T) {
	_, err := NewWireGuard(nil, mustKey(t), config.WireGuardConfig{}, discardLogger(), 0)
	if !errors.Is(err, ErrNilFabric) {
		t.Errorf("err = %v, want ErrNilFabric", err)
	}
}

func TestIsKnownNodeOverlayIP(t *testing.T) {
	r, err := NewWireGuard(&netfabric.FakeFabric{}, mustKey(t), config.WireGuardConfig{}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}

	// Fail closed before any heartbeat populates the peer set.
	if r.IsKnownNodeOverlayIP(netip.MustParseAddr("10.0.0.9")) {
		t.Error("IsKnownNodeOverlayIP before first heartbeat = true, want false (fail closed)")
	}

	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredWireGuardPeers: []heartbeat.DeclaredWireGuardPeer{
			{NodeID: "n9", PublicKey: "p9", OverlayIP: "10.0.0.9"},
			{NodeID: "n-empty", PublicKey: "pe", OverlayIP: ""}, // skipped
		},
	})

	if !r.IsKnownNodeOverlayIP(netip.MustParseAddr("10.0.0.9")) {
		t.Error("declared peer overlay IP = false, want true")
	}
	// A guest/unknown IP and the anycast service IP must not be known.
	if r.IsKnownNodeOverlayIP(netip.MustParseAddr("10.0.0.50")) {
		t.Error("unknown IP = true, want false")
	}
	if r.IsKnownNodeOverlayIP(netip.MustParseAddr("169.254.1.1")) {
		t.Error("anycast IP = true, want false")
	}
}

func TestSelfOverlayIP(t *testing.T) {
	r, err := NewWireGuard(&netfabric.FakeFabric{}, mustKey(t), config.WireGuardConfig{}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}

	// Fail closed before any heartbeat delivers self_overlay_ip.
	if ip, ok := r.SelfOverlayIP(); ok {
		t.Errorf("SelfOverlayIP before first heartbeat = (%v, true), want (_, false)", ip)
	}

	cidr := "10.42.0.7/16"
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &cidr})

	got, ok := r.SelfOverlayIP()
	if !ok {
		t.Fatal("SelfOverlayIP after heartbeat: ok = false, want true")
	}
	if got != netip.MustParseAddr("10.42.0.7") {
		t.Errorf("SelfOverlayIP() = %v, want 10.42.0.7", got)
	}

	// A malformed delivered value fails closed (mirrors the reconcile skip).
	bad := "not-a-prefix"
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &bad})
	if ip, ok := r.SelfOverlayIP(); ok {
		t.Errorf("SelfOverlayIP with malformed value = (%v, true), want (_, false)", ip)
	}
}

func TestWireGuardReport_IndependentOfInterface(t *testing.T) {
	key := mustKey(t)
	cfg := config.WireGuardConfig{ListenPort: 51820, AdvertisedEndpoint: "1.2.3.4:51820"}
	r, err := NewWireGuard(&netfabric.FakeFabric{}, key, cfg, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	rep := r.WireGuardReport()
	if rep == nil {
		t.Fatal("WireGuardReport() = nil, want report")
	}
	if rep.PublicKey != key.PublicKey().String() {
		t.Errorf("PublicKey = %s, want %s", rep.PublicKey, key.PublicKey())
	}
	if rep.Endpoint != "1.2.3.4:51820" || rep.ListenPort != 51820 {
		t.Errorf("report endpoint/port = %s/%d, want 1.2.3.4:51820/51820", rep.Endpoint, rep.ListenPort)
	}
}

func TestWireGuardReconcile_BringsUpInterfaceWhenOverlayKnown(t *testing.T) {
	key := mustKey(t)
	f := &netfabric.FakeFabric{}
	r, err := NewWireGuard(f, key, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	ip := "10.42.0.1/16"
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	r.reconcile(context.Background())

	if len(f.EnsureWireGuardCalls) != 1 {
		t.Fatalf("EnsureWireGuardCalls = %d, want 1", len(f.EnsureWireGuardCalls))
	}
	got := f.EnsureWireGuardCalls[0]
	if got.Name != "otwg0" {
		t.Errorf("Name = %s, want otwg0", got.Name)
	}
	if got.Address.String() != "10.42.0.1/16" {
		t.Errorf("Address = %s, want 10.42.0.1/16", got.Address)
	}
	if got.ListenPort != 51820 {
		t.Errorf("ListenPort = %d, want 51820", got.ListenPort)
	}
	if len(f.WGPeerSetCalls) != 1 || len(f.WGPeerSetCalls[0].Peers) != 0 {
		t.Errorf("WGPeerSetCalls = %+v, want one call with zero peers", f.WGPeerSetCalls)
	}
}

func TestWireGuardReconcileSetsOverlayMTU(t *testing.T) {
	f := &netfabric.FakeFabric{}
	rec, err := NewWireGuard(f, wgtypes.Key{}, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	ip := "10.42.0.7/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	rec.reconcile(context.Background())

	if len(f.EnsureWireGuardCalls) != 1 {
		t.Fatalf("EnsureWireGuardCalls = %d, want 1", len(f.EnsureWireGuardCalls))
	}
	if got := f.EnsureWireGuardCalls[0].MTU; got != netfabric.WireGuardMTU {
		t.Errorf("otwg0 MTU = %d, want %d", got, netfabric.WireGuardMTU)
	}
}

func TestWireGuardUsesDeclaredMTU(t *testing.T) {
	f := &netfabric.FakeFabric{}
	rec, err := NewWireGuard(f, wgtypes.Key{}, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	ip := "10.42.0.7/16"
	mtu := int32(1380)
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		SelfOverlayIP: &ip,
		Otwg0MTU:      &mtu,
	})
	rec.reconcile(context.Background())
	if len(f.EnsureWireGuardCalls) == 0 {
		t.Fatal("EnsureWireGuard not called")
	}
	if f.EnsureWireGuardCalls[0].MTU != 1380 {
		t.Errorf("otwg0 MTU = %d, want the declared 1380", f.EnsureWireGuardCalls[0].MTU)
	}
}

func TestWireGuardReconcile_WaitsForOverlayIP(t *testing.T) {
	f := &netfabric.FakeFabric{}
	r, err := NewWireGuard(f, mustKey(t), config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{}) // no self_overlay_ip
	r.reconcile(context.Background())
	if len(f.EnsureWireGuardCalls) != 0 {
		t.Errorf("EnsureWireGuardCalls = %d, want 0 (interface stays down until overlay IP arrives)", len(f.EnsureWireGuardCalls))
	}
}

func TestWireGuardReportEstablishedPeers(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	peerKey, _ := wgtypes.GeneratePrivateKey()
	r, err := NewWireGuard(&netfabric.FakeFabric{
		WireGuardPeerHandshakesResult: []netfabric.WGPeerHandshake{
			{PublicKey: peerKey.PublicKey(), LastHandshake: time.Unix(1000, 0)},
		},
	}, key, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewWireGuard() error = %v", err)
	}
	r.now = func() time.Time { return time.Unix(1100, 0) } // 100s after handshake, within 180s
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredWireGuardPeers: []heartbeat.DeclaredWireGuardPeer{
			{NodeID: "node-b", PublicKey: peerKey.PublicKey().String()},
		},
	})
	rep := r.WireGuardReport()
	if len(rep.EstablishedPeers) != 1 || rep.EstablishedPeers[0] != "node-b" {
		t.Errorf("EstablishedPeers = %v, want [node-b]", rep.EstablishedPeers)
	}
}

func TestWireGuardReportEstablishedStaleAndZero(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	stalePeer, _ := wgtypes.GeneratePrivateKey()
	zeroPeer, _ := wgtypes.GeneratePrivateKey()
	r, _ := NewWireGuard(&netfabric.FakeFabric{
		WireGuardPeerHandshakesResult: []netfabric.WGPeerHandshake{
			{PublicKey: stalePeer.PublicKey(), LastHandshake: time.Unix(1000, 0)}, // 200s stale
			{PublicKey: zeroPeer.PublicKey()},                                     // never handshook
		},
	}, key, config.WireGuardConfig{}, discardLogger(), time.Second)
	r.now = func() time.Time { return time.Unix(1200, 0) }
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredWireGuardPeers: []heartbeat.DeclaredWireGuardPeer{
			{NodeID: "stale", PublicKey: stalePeer.PublicKey().String()},
			{NodeID: "zero", PublicKey: zeroPeer.PublicKey().String()},
		},
	})
	if got := r.WireGuardReport().EstablishedPeers; len(got) != 0 {
		t.Errorf("EstablishedPeers = %v, want none (stale + zero excluded)", got)
	}
}

func TestWireGuardReportEstablishedFabricError(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	r, _ := NewWireGuard(&netfabric.FakeFabric{
		Errs: map[string]error{"WireGuardPeerHandshakes": errors.New("no device")},
	}, key, config.WireGuardConfig{}, discardLogger(), time.Second)
	rep := r.WireGuardReport()
	if rep.PublicKey == "" {
		t.Errorf("PublicKey empty; report must survive a fabric read error")
	}
	if len(rep.EstablishedPeers) != 0 {
		t.Errorf("EstablishedPeers = %v, want none on fabric error", rep.EstablishedPeers)
	}
}

func TestWireGuardReport_ReconciliationStatusFailed(t *testing.T) {
	key := mustKey(t)
	f := &netfabric.FakeFabric{Errs: map[string]error{"EnsureWireGuard": errors.New("link down")}}
	r, err := NewWireGuard(f, key, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	ip := "10.42.0.1/16"
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	r.reconcile(context.Background())

	rep := r.WireGuardReport()
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("ReconciliationStatus = %q, want failed", rep.ReconciliationStatus)
	}
	if rep.ReconciliationError == nil || *rep.ReconciliationError == "" {
		t.Errorf("ReconciliationError = %v, want non-empty failure message", rep.ReconciliationError)
	}
}

func TestWireGuardReport_ReconciliationStatusReady(t *testing.T) {
	key := mustKey(t)
	r, err := NewWireGuard(&netfabric.FakeFabric{}, key, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	ip := "10.42.0.1/16"
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	r.reconcile(context.Background())

	rep := r.WireGuardReport()
	if rep.ReconciliationStatus != "ready" {
		t.Errorf("ReconciliationStatus = %q, want ready", rep.ReconciliationStatus)
	}
	if rep.ReconciliationError != nil {
		t.Errorf("ReconciliationError = %v, want nil on ready", rep.ReconciliationError)
	}
}

func TestWireGuardReport_ReconciliationStatusPending(t *testing.T) {
	key := mustKey(t)
	r, err := NewWireGuard(&netfabric.FakeFabric{}, key, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	// No overlay IP yet: the interface stays down and the status is pending.
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{})
	r.reconcile(context.Background())

	rep := r.WireGuardReport()
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("ReconciliationStatus = %q, want pending", rep.ReconciliationStatus)
	}
	if rep.ReconciliationError != nil {
		t.Errorf("ReconciliationError = %v, want nil on pending", rep.ReconciliationError)
	}
}

func TestWireGuardReport_ReconciliationStatusDefaultsPending(t *testing.T) {
	key := mustKey(t)
	r, err := NewWireGuard(&netfabric.FakeFabric{}, key, config.WireGuardConfig{ListenPort: 51820}, discardLogger(), time.Second)
	if err != nil {
		t.Fatalf("NewWireGuard: %v", err)
	}
	// Before the first reconcile pass the status defaults to pending.
	rep := r.WireGuardReport()
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("ReconciliationStatus = %q, want pending before first reconcile", rep.ReconciliationStatus)
	}
}

func TestToWGPeersSkipsUnparseable(t *testing.T) {
	key, _ := wgtypes.GeneratePrivateKey()
	good, _ := wgtypes.GeneratePrivateKey()
	r, _ := NewWireGuard(&netfabric.FakeFabric{}, key, config.WireGuardConfig{}, discardLogger(), time.Second)
	peers := r.toWGPeers(context.Background(), []heartbeat.DeclaredWireGuardPeer{
		{NodeID: "bad-key", PublicKey: "not-a-key"},
		{NodeID: "bad-ep", PublicKey: good.PublicKey().String(), Endpoint: "::::"},
		{NodeID: "ok", PublicKey: good.PublicKey().String(), AllowedIPs: []string{"10.42.0.2/32"}},
	})
	if len(peers) != 1 {
		t.Fatalf("toWGPeers kept %d peers, want 1 (bad key + bad endpoint skipped)", len(peers))
	}
	if got := peers[0].PublicKey; got != good.PublicKey() {
		t.Errorf("kept peer pubkey = %v, want %v", got, good.PublicKey())
	}
}
