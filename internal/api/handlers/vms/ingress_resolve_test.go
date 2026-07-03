// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/gateways"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// resolveIngressStoreStub backs the ResolveIngress tests. It serves the NIC
// list, the network lookup, the gateway-selection reads, and the active
// session CA. Any unconfigured method panics (embedded nil Store), proving the
// resolver bailed before reaching it.
type resolveIngressStoreStub struct {
	Store
	nics         []store.VMNic
	networks     map[uuid.UUID]store.Network
	nodes        []store.Node
	memberships  map[uuid.UUID][]store.GatewayMembership
	nodeStatuses map[uuid.UUID][]store.NetworkNodeStatus
	sessionCA    store.SessionCA
	sessionCAErr error
}

func (s *resolveIngressStoreStub) ListVMNicsByVM(context.Context, uuid.UUID) ([]store.VMNic, error) {
	return s.nics, nil
}

func (s *resolveIngressStoreStub) NetworkByID(_ context.Context, id uuid.UUID) (store.Network, error) {
	net, ok := s.networks[id]
	if !ok {
		return store.Network{}, store.ErrNotFound
	}
	return net, nil
}

func (s *resolveIngressStoreStub) AllNodes(context.Context) ([]store.Node, error) {
	return s.nodes, nil
}

func (s *resolveIngressStoreStub) ListGatewayMembershipsForNetwork(_ context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error) {
	return s.memberships[networkID], nil
}

func (s *resolveIngressStoreStub) ListNetworkNodeStatusByNetwork(_ context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	return s.nodeStatuses[networkID], nil
}

func (s *resolveIngressStoreStub) ActiveSessionCA(context.Context) (store.SessionCA, error) {
	return s.sessionCA, s.sessionCAErr
}

// seedResolveIngressOverlay builds a handler over a running overlay VM with an
// IPv4 NIC. When withGateway is true a live gateway node is a converged member
// of the VM's overlay, so ResolveIngress can select it; otherwise no gateway
// covers the overlay and selection fails with ErrIngressUnavailable.
func seedResolveIngressOverlay(t *testing.T, withGateway bool) (*Handler, store.VM) {
	t.Helper()

	overlayNet := store.Network{ID: uuid.New(), Name: "ov0", Type: store.NetworkTypeOverlay}
	vm := store.VM{ID: uuid.New(), Name: "web01", OwnerID: uuid.New()}
	ip := netip.MustParseAddr("10.20.0.5")
	mac, err := net.ParseMAC("52:54:00:12:34:56")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	caMaterial, err := auth.GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA: %v", err)
	}

	st := &resolveIngressStoreStub{
		nics: []store.VMNic{{
			ID:          uuid.New(),
			VmID:        vm.ID,
			NetworkID:   overlayNet.ID,
			MacAddress:  mac,
			Ipv4Address: &ip,
		}},
		networks:     map[uuid.UUID]store.Network{overlayNet.ID: overlayNet},
		memberships:  map[uuid.UUID][]store.GatewayMembership{},
		nodeStatuses: map[uuid.UUID][]store.NetworkNodeStatus{},
		sessionCA:    store.SessionCA{ID: uuid.New(), PrivateKeyPEM: caMaterial.PrivateKeyPEM},
	}

	if withGateway {
		gw := store.Node{
			ID:                        uuid.New(),
			Name:                      "gw0",
			GatewayRole:               true,
			Status:                    store.NodeStatusReady,
			AdvertisedEndpoint:        "https://gw0.example.com:9443",
			IngressAdvertisedEndpoint: "https://gw0.example.com:9444",
		}
		st.nodes = []store.Node{gw}
		st.memberships[overlayNet.ID] = []store.GatewayMembership{{GatewayID: gw.ID, NetworkID: overlayNet.ID}}
		st.nodeStatuses[overlayNet.ID] = []store.NetworkNodeStatus{{
			NetworkID:            overlayNet.ID,
			NodeID:               gw.ID,
			ReconciliationStatus: "ready",
		}}
	}

	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)),
		LifecycleDeps{}, ConsoleDeps{}, SSHDeps{})
	return h, vm
}

// TestResolveIngressOverlayReturnsGatewayCoords: a running overlay VM with an
// IPv4 NIC and a converged gateway resolves to transport "gateway" with a
// non-empty SplicerAddr + SessionCred and the VM's name.
func TestResolveIngressOverlayReturnsGatewayCoords(t *testing.T) {
	t.Parallel()
	h, vm := seedResolveIngressOverlay(t, true)

	got, err := h.ResolveIngress(context.Background(), vm, 22)
	if err != nil {
		t.Fatalf("ResolveIngress() error = %v, want nil", err)
	}
	if got.Transport != "gateway" {
		t.Errorf("Transport = %q, want gateway", got.Transport)
	}
	if got.SessionCred == "" || got.SplicerAddr == "" {
		t.Errorf("gateway coords empty: cred=%q addr=%q", got.SessionCred, got.SplicerAddr)
	}
	if got.VMName != vm.Name {
		t.Errorf("VMName = %q, want %q", got.VMName, vm.Name)
	}
	// The client pins the gateway TLS ServerName to the node identity SAN, not
	// the dialed ingress IP, so the broker must surface the gateway node's
	// identity (node-<name>.agents.otherix.local) alongside SplicerAddr.
	if want := auth.NodeIdentitySAN("gw0"); got.SplicerServerName != want {
		t.Errorf("SplicerServerName = %q, want %q", got.SplicerServerName, want)
	}
}

// TestResolveIngressNoGatewayUnavailable: a running overlay VM with NO converged
// gateway returns gateways.ErrIngressUnavailable and empty coords.
func TestResolveIngressNoGatewayUnavailable(t *testing.T) {
	t.Parallel()
	h, vm := seedResolveIngressOverlay(t, false)

	got, err := h.ResolveIngress(context.Background(), vm, 22)
	if !errors.Is(err, gateways.ErrIngressUnavailable) {
		t.Fatalf("ResolveIngress() error = %v, want ErrIngressUnavailable", err)
	}
	if got.Transport != "" || got.SplicerAddr != "" || got.SessionCred != "" || got.SplicerServerName != "" {
		t.Errorf("want empty coords on unavailable, got %+v", got)
	}
}
