// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentroute_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/agentroute"
	"github.com/otherix/otherix/internal/store"
)

// fakeReader is an in-memory NodeReader.
type fakeReader struct {
	nodes map[string]store.Node    // by name
	byID  map[uuid.UUID]store.Node // by id
	wg    map[uuid.UUID]store.AgentWireguard
	wgErr error // when non-nil, AgentWireguardByNodeID returns it (fault injection)
}

func (f *fakeReader) NodeByName(_ context.Context, name string) (store.Node, error) {
	n, ok := f.nodes[name]
	if !ok {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

func (f *fakeReader) AgentWireguardByNodeID(_ context.Context, id uuid.UUID) (store.AgentWireguard, error) {
	if f.wgErr != nil {
		return store.AgentWireguard{}, f.wgErr
	}
	w, ok := f.wg[id]
	if !ok {
		return store.AgentWireguard{}, store.ErrNotFound
	}
	return w, nil
}

func (f *fakeReader) AllNodes(_ context.Context) ([]store.Node, error) {
	out := make([]store.Node, 0, len(f.byID))
	for _, n := range f.byID {
		out = append(out, n)
	}
	return out, nil
}

func newReader(nodes ...store.Node) *fakeReader {
	f := &fakeReader{
		nodes: map[string]store.Node{},
		byID:  map[uuid.UUID]store.Node{},
		wg:    map[uuid.UUID]store.AgentWireguard{},
	}
	for _, n := range nodes {
		f.nodes[n.Name] = n
		f.byID[n.ID] = n
	}
	return f
}

func TestResolve_PublicNode_Direct(t *testing.T) {
	pub := store.Node{ID: uuid.New(), Name: "pub", AdvertisedEndpoint: "https://5.5.5.5:9443"}
	f := newReader(pub)
	f.wg[pub.ID] = store.AgentWireguard{NodeID: pub.ID, Endpoint: "5.5.5.5:51820"}

	got, err := agentroute.New(f).Resolve("pub")
	if err != nil {
		t.Fatalf("Resolve(pub) error: %v", err)
	}
	want := agentclient.AgentRoute{Direct: "5.5.5.5:9443"}
	if got != want {
		t.Errorf("Resolve(pub) = %+v, want %+v", got, want)
	}
}

func TestResolve_NoWGRow_TreatedPublic(t *testing.T) {
	// A node with no WireGuard state is not confirmed NAT'd; dial it directly.
	pub := store.Node{ID: uuid.New(), Name: "fresh", AdvertisedEndpoint: "https://1.2.3.4:9443"}
	got, err := agentroute.New(newReader(pub)).Resolve("fresh")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if got.Direct != "1.2.3.4:9443" || got.GatewayDial != "" {
		t.Errorf("Resolve(fresh) = %+v, want Direct=1.2.3.4:9443", got)
	}
}

func TestResolve_WGStoreError_FailsClosed(t *testing.T) {
	// A non-NotFound store error reading WG state must fail closed: return the
	// error, never fold into the "public node -> Direct dial" branch (which would
	// mis-route a possibly-NAT'd node and swallow the fault).
	node := store.Node{ID: uuid.New(), Name: "n", AdvertisedEndpoint: "https://1.2.3.4:9443"}
	f := newReader(node)
	f.wgErr = errors.New("etcd unavailable")

	got, err := agentroute.New(f).Resolve("n")
	if err == nil {
		t.Fatalf("Resolve = %+v, nil error; want a fail-closed error on a transient WG store error", got)
	}
	if (got != agentclient.AgentRoute{}) {
		t.Errorf("Resolve returned route %+v alongside the error, want the zero route", got)
	}
}

func TestResolve_PortlessEndpoint_DefaultsControlPort(t *testing.T) {
	// A public node whose advertised endpoint carries no explicit port must dial
	// the agent control-listener default (9443), not 443.
	pub := store.Node{ID: uuid.New(), Name: "noport", AdvertisedEndpoint: "https://5.5.5.5"}

	got, err := agentroute.New(newReader(pub)).Resolve("noport")
	if err != nil {
		t.Fatalf("Resolve(noport) error: %v", err)
	}
	if got.Direct != "5.5.5.5:9443" {
		t.Errorf("Resolve(noport).Direct = %q, want 5.5.5.5:9443", got.Direct)
	}
}

func TestResolve_UnknownNode_ZeroRoute(t *testing.T) {
	// A host that is not a known node falls back to the transport's computed addr.
	got, err := agentroute.New(newReader()).Resolve("1.2.3.4")
	if err != nil {
		t.Fatalf("Resolve unknown error: %v", err)
	}
	if (got != agentclient.AgentRoute{}) {
		t.Errorf("Resolve(unknown) = %+v, want zero route", got)
	}
}

func TestResolve_NATd_RoutesThroughLowestUUIDGateway(t *testing.T) {
	natd := store.Node{ID: uuid.New(), Name: "edge", AdvertisedEndpoint: "https://10.0.0.9:9443"}
	// Two reachable gateways; the lower-UUID one must win.
	gwHi := store.Node{ID: uuid.MustParse("ffffffff-0000-0000-0000-000000000000"), Name: "gw-hi", GatewayRole: true, AdvertisedEndpoint: "https://hi.gw:9443"}
	gwLo := store.Node{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: "gw-lo", GatewayRole: true, AdvertisedEndpoint: "https://lo.gw:9443"}
	f := newReader(natd, gwHi, gwLo)
	f.wg[natd.ID] = store.AgentWireguard{
		NodeID:           natd.ID,
		Endpoint:         "", // NAT'd
		OverlayIP:        netip.MustParseAddr("10.100.0.9"),
		EstablishedPeers: []string{gwHi.ID.String(), gwLo.ID.String()},
	}

	got, err := agentroute.New(f).Resolve("edge")
	if err != nil {
		t.Fatalf("Resolve(edge) error: %v", err)
	}
	want := agentclient.AgentRoute{
		GatewayDial:   "lo.gw:9443",
		GatewaySAN:    "node-gw-lo.agents.otherix.local",
		TargetOverlay: "10.100.0.9:9443",
	}
	if got != want {
		t.Errorf("Resolve(edge) = %+v, want %+v", got, want)
	}
}

func TestResolve_NATd_UnreachedGateway_NotSelected(t *testing.T) {
	natd := store.Node{ID: uuid.New(), Name: "edge", AdvertisedEndpoint: "https://10.0.0.9:9443"}
	// Gateway exists but the NAT'd node does not currently reach it.
	gw := store.Node{ID: uuid.New(), Name: "gw", GatewayRole: true, AdvertisedEndpoint: "https://gw:9443"}
	f := newReader(natd, gw)
	f.wg[natd.ID] = store.AgentWireguard{
		NodeID:           natd.ID,
		Endpoint:         "",
		OverlayIP:        netip.MustParseAddr("10.100.0.9"),
		EstablishedPeers: nil, // reaches nobody
	}

	if _, err := agentroute.New(f).Resolve("edge"); err == nil {
		t.Error("Resolve(NAT'd node with no reachable gateway) = nil error, want error (no route)")
	}
}

func TestResolve_NATd_NonGatewayPeer_NotSelected(t *testing.T) {
	natd := store.Node{ID: uuid.New(), Name: "edge", AdvertisedEndpoint: "https://10.0.0.9:9443"}
	peer := store.Node{ID: uuid.New(), Name: "peer", GatewayRole: false, AdvertisedEndpoint: "https://peer:9443"}
	f := newReader(natd, peer)
	f.wg[natd.ID] = store.AgentWireguard{
		NodeID:           natd.ID,
		Endpoint:         "",
		OverlayIP:        netip.MustParseAddr("10.100.0.9"),
		EstablishedPeers: []string{peer.ID.String()}, // reaches a non-gateway only
	}

	if _, err := agentroute.New(f).Resolve("edge"); err == nil {
		t.Error("Resolve(NAT'd reaching only a non-gateway) = nil error, want error")
	}
}
