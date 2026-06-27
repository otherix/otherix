// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	networkshandlers "github.com/otherix/otherix/internal/api/handlers/networks"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd-backed store satisfies the networks handler's storage contract -
// the narrow interface the handler depends on.
var _ networkshandlers.Store = (*etcdstore.Store)(nil)

// startStore returns a Store over the shared embedded member with a freshly
// wiped keyspace (see FreshStore / TestMain in main_test.go), plus the raw
// client so tests can seed index keys directly.
func startStore(t *testing.T) (*etcdstore.Store, *etcd.Client) {
	t.Helper()
	return etcdstore.FreshStore(t)
}

func netParams(name string) store.CreateNetworkParams {
	return store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       name,
		Type:       store.NetworkTypeBridge,
		BridgeName: "br0",
		Mtu:        1500,
		Config:     []byte(`{}`),
	}
}

func uniqueNetName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestNetworkCreateAndGet(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	p := netParams(uniqueNetName("net"))
	created, err := s.CreateNetwork(ctx, p)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if created.ID != p.ID || created.Name != p.Name || created.CreatedAt.IsZero() {
		t.Errorf("CreateNetwork returned %+v, want id/name set + created_at stamped", created)
	}
	got, err := s.NetworkByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("NetworkByID: %v", err)
	}
	if got.ID != p.ID || got.Name != p.Name {
		t.Errorf("NetworkByID = %+v, want id=%v name=%q", got, p.ID, p.Name)
	}
}

func TestNetworkByIDNotFound(t *testing.T) {
	s, _ := startStore(t)
	if _, err := s.NetworkByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByID(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestNetworkCreateDuplicateName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueNetName("dup")
	if _, err := s.CreateNetwork(ctx, netParams(name)); err != nil {
		t.Fatalf("first CreateNetwork: %v", err)
	}
	// Different casing must still collide (case-insensitive guard).
	clash := netParams(strings.ToUpper(name))
	_, err := s.CreateNetwork(ctx, clash)
	if !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("duplicate name err = %v, want store.ErrNetworkNameExists", err)
	}
}

func TestNetworkUpdateRenameCollision(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	taken := uniqueNetName("taken")
	if _, err := s.CreateNetwork(ctx, netParams(taken)); err != nil {
		t.Fatalf("seed taken: %v", err)
	}
	mover := netParams(uniqueNetName("mover"))
	if _, err := s.CreateNetwork(ctx, mover); err != nil {
		t.Fatalf("seed mover: %v", err)
	}
	_, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID: mover.ID, Name: taken, BridgeName: "br1", Mtu: 1500, Config: []byte(`{}`),
	})
	if !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("rename collision err = %v, want store.ErrNetworkNameExists", err)
	}
}

func TestNetworkUpdateSucceeds(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("upd"))
	created, err := s.CreateNetwork(ctx, p)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	newName := uniqueNetName("upd-renamed")
	updated, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID: p.ID, Name: newName, BridgeName: "br9", Mtu: 9000, Config: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if updated.Name != newName || updated.BridgeName != "br9" || updated.Mtu != 9000 {
		t.Errorf("UpdateNetwork = %+v, want renamed/br9/9000", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at not bumped: created %v updated %v", created.UpdatedAt, updated.UpdatedAt)
	}
	// Old name is free again; the new name now collides.
	if _, err := s.CreateNetwork(ctx, netParams(p.Name)); err != nil {
		t.Errorf("old name not reusable after rename: %v", err)
	}
	if _, err := s.CreateNetwork(ctx, netParams(newName)); !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("new name should be taken, got %v", err)
	}
}

func TestNetworkListOrderingPaginationAndDeletedExcluded(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	ids := make([]uuid.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		p := netParams(uniqueNetName(fmt.Sprintf("list%d", i)))
		if _, err := s.CreateNetwork(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, p.ID)
		time.Sleep(2 * time.Millisecond) // keep created_at strictly increasing
	}

	all, err := s.ListNetworks(ctx, store.ListNetworksParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListNetworks all: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("ListNetworks returned %d, want >= 3", len(all))
	}
	// Ascending (created_at, id).
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Errorf("list not ascending at %d: %v before %v", i, all[i].CreatedAt, all[i-1].CreatedAt)
		}
	}

	// Pagination: limit 1 from the first row's cursor yields the next row.
	first := all[0]
	page, err := s.ListNetworks(ctx, store.ListNetworksParams{
		CursorCreatedAt: &first.CreatedAt, CursorID: &first.ID, LimitCount: 1,
	})
	if err != nil {
		t.Fatalf("ListNetworks paged: %v", err)
	}
	if len(page) != 1 || page[0].ID != all[1].ID {
		t.Errorf("paged after first = %v, want [%v]", idsOf(page), all[1].ID)
	}

	// Soft-deleted rows drop out of the list.
	if err := s.DeleteNetwork(ctx, ids[0]); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	after, err := s.ListNetworks(ctx, store.ListNetworksParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListNetworks after delete: %v", err)
	}
	for _, n := range after {
		if n.ID == ids[0] {
			t.Errorf("deleted network %v still listed", ids[0])
		}
	}
}

func TestNetworkDeleteAndNameReuse(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueNetName("del")
	p := netParams(name)
	if _, err := s.CreateNetwork(ctx, p); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if err := s.DeleteNetwork(ctx, p.ID); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, err := s.NetworkByID(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByID after delete = %v, want store.ErrNotFound", err)
	}
	// Name reusable after soft-delete (guard dropped).
	if _, err := s.CreateNetwork(ctx, netParams(name)); err != nil {
		t.Errorf("name not reusable after delete: %v", err)
	}
}

func TestNetworkDeleteBlockedByNic(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("blk"))
	if _, err := s.CreateNetwork(ctx, p); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	// Seed an active vm_nic referencing the network: the per-network index entry
	// plus a live NIC row it resolves to (countVMNicsOnNetwork reconciles the
	// index against live rows, so a row-less entry would not block).
	nicID := uuid.New()
	nicRow := store.VMNic{
		ID: nicID, VmID: uuid.New(), NetworkID: p.ID, DeviceOrder: 0,
		Model: store.NicModelVirtio, Generation: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("vm_nics", nicID.String()), nicRow); err != nil {
		t.Fatalf("seed nic row: %v", err)
	}
	nicKey := etcd.Key("index", "vm_nics", "network", p.ID.String(), nicID.String())
	if err := cli.Put(ctx, nicKey, []byte(nicID.String())); err != nil {
		t.Fatalf("seed nic index: %v", err)
	}

	err := s.DeleteNetwork(ctx, p.ID)
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		t.Fatalf("DeleteNetwork err = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vm_nics"] != 1 {
		t.Errorf("blocking vm_nics = %d, want 1", blocking.Resources["vm_nics"])
	}
}

func TestNetworkManagedFieldsRoundTrip(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	subnet := netip.MustParsePrefix("10.42.0.0/24")
	gateway := netip.MustParseAddr("10.42.0.1")
	p := netParams(uniqueNetName("managed"))
	p.Managed = true
	p.Egress = store.NetworkEgressNAT
	p.Subnet = &subnet
	p.Gateway = &gateway

	created, err := s.CreateNetwork(ctx, p)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	want := managedView{Managed: true, Egress: store.NetworkEgressNAT, Subnet: subnet.String(), Gateway: gateway.String()}
	if diff := cmp.Diff(want, viewOf(created)); diff != "" {
		t.Errorf("CreateNetwork managed fields mismatch (-want +got):\n%s", diff)
	}
	got, err := s.NetworkByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("NetworkByID: %v", err)
	}
	if diff := cmp.Diff(want, viewOf(got)); diff != "" {
		t.Errorf("NetworkByID managed fields mismatch (-want +got):\n%s", diff)
	}
}

func TestNetworkCreateEgressDefaultsToNone(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("egress-default"))
	// Egress left empty: store defaults it to "none" defensively.
	created, err := s.CreateNetwork(ctx, p)
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if created.Egress != store.NetworkEgressNone {
		t.Errorf("Egress = %q, want %q", created.Egress, store.NetworkEgressNone)
	}
}

func TestNetworkUpdateEgressEmptyDefaultsToNone(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("upd-egress-empty"))
	if _, err := s.CreateNetwork(ctx, p); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	// Egress left empty on update: store defaults it to "none" defensively so
	// an out-of-enum value never reaches the wire.
	updated, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID: p.ID, Name: p.Name, BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if updated.Egress != store.NetworkEgressNone {
		t.Errorf("Egress = %q, want %q", updated.Egress, store.NetworkEgressNone)
	}
}

func TestNetworkUpdateManagedFieldsRoundTrip(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := netParams(uniqueNetName("upd-managed"))
	if _, err := s.CreateNetwork(ctx, p); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	subnet := netip.MustParsePrefix("192.168.5.0/24")
	gateway := netip.MustParseAddr("192.168.5.1")
	updated, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID:         p.ID,
		Name:       p.Name,
		BridgeName: "br0",
		Mtu:        1500,
		Config:     []byte(`{}`),
		Managed:    true,
		Egress:     store.NetworkEgressNAT,
		Subnet:     &subnet,
		Gateway:    &gateway,
	})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	want := managedView{Managed: true, Egress: store.NetworkEgressNAT, Subnet: subnet.String(), Gateway: gateway.String()}
	if diff := cmp.Diff(want, viewOf(updated)); diff != "" {
		t.Errorf("UpdateNetwork managed fields mismatch (-want +got):\n%s", diff)
	}
	got, err := s.NetworkByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("NetworkByID: %v", err)
	}
	if diff := cmp.Diff(want, viewOf(got)); diff != "" {
		t.Errorf("NetworkByID managed fields mismatch (-want +got):\n%s", diff)
	}
}

func TestNetworkNodeStatusUpsertAndList(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	net := netParams(uniqueNetName("nns"))
	if _, err := s.CreateNetwork(ctx, net); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	nodeA := uuid.New()
	nodeB := uuid.New()
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: net.ID, NodeID: nodeA, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus A: %v", err)
	}
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: net.ID, NodeID: nodeB, ReconciliationStatus: "pending",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus B: %v", err)
	}

	byNet, err := s.ListNetworkNodeStatusByNetwork(ctx, net.ID)
	if err != nil {
		t.Fatalf("ListNetworkNodeStatusByNetwork: %v", err)
	}
	if len(byNet) != 2 {
		t.Fatalf("ListNetworkNodeStatusByNetwork len = %d, want 2", len(byNet))
	}
	// Sorted by NodeID string.
	if byNet[0].NodeID.String() > byNet[1].NodeID.String() {
		t.Errorf("ListNetworkNodeStatusByNetwork not sorted by node id: %v", byNet)
	}
	for _, st := range byNet {
		if st.UpdatedAt.IsZero() {
			t.Errorf("status %+v missing updated_at", st)
		}
		// last_reconciled_at tracks the last successful reconciliation: set for
		// the ready node, nil for the pending node (never reconciled).
		switch st.NodeID {
		case nodeA:
			if st.LastReconciledAt == nil {
				t.Errorf("ready node %v last_reconciled_at = nil, want stamped", nodeA)
			}
		case nodeB:
			if st.LastReconciledAt != nil {
				t.Errorf("pending node %v last_reconciled_at = %v, want nil", nodeB, st.LastReconciledAt)
			}
		}
	}

	byNode, err := s.ListNetworkNodeStatusByNode(ctx, nodeA)
	if err != nil {
		t.Fatalf("ListNetworkNodeStatusByNode: %v", err)
	}
	if len(byNode) != 1 || byNode[0].NodeID != nodeA || byNode[0].NetworkID != net.ID {
		t.Errorf("ListNetworkNodeStatusByNode(A) = %+v, want single (net,A)", byNode)
	}

	// Upsert again for (net, A) with a failed status + error reflects last-writer-wins.
	failMsg := "boom"
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: net.ID, NodeID: nodeA, ReconciliationStatus: "failed", ReconciliationError: &failMsg,
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus re-A: %v", err)
	}
	again, err := s.ListNetworkNodeStatusByNode(ctx, nodeA)
	if err != nil {
		t.Fatalf("ListNetworkNodeStatusByNode re-A: %v", err)
	}
	if len(again) != 1 || again[0].ReconciliationStatus != "failed" ||
		again[0].ReconciliationError == nil || *again[0].ReconciliationError != failMsg {
		t.Errorf("re-upsert (net,A) = %+v, want failed/%q", again, failMsg)
	}
}

// TestNetworkNodeStatusLastReconciledOnlyOnReady verifies last_reconciled_at
// tracks the last SUCCESSFUL reconciliation: an upsert with status "ready"
// stamps it, a subsequent "failed" upsert preserves the prior timestamp
// (success time, not the failure time) while still advancing updated_at and the
// status.
func TestNetworkNodeStatusLastReconciledOnlyOnReady(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	net := netParams(uniqueNetName("lra"))
	if _, err := s.CreateNetwork(ctx, net); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	nodeID := uuid.New()

	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: net.ID, NodeID: nodeID, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus ready: %v", err)
	}
	ready, err := s.ListNetworkNodeStatusByNetwork(ctx, net.ID)
	if err != nil || len(ready) != 1 {
		t.Fatalf("list after ready = %v (err %v), want one row", ready, err)
	}
	if ready[0].LastReconciledAt == nil {
		t.Fatalf("ready last_reconciled_at = nil, want stamped")
	}
	readyAt := *ready[0].LastReconciledAt
	readyUpdated := ready[0].UpdatedAt

	time.Sleep(2 * time.Millisecond)
	failMsg := "bridge gone"
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: net.ID, NodeID: nodeID, ReconciliationStatus: "failed", ReconciliationError: &failMsg,
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus failed: %v", err)
	}
	failed, err := s.ListNetworkNodeStatusByNetwork(ctx, net.ID)
	if err != nil || len(failed) != 1 {
		t.Fatalf("list after failed = %v (err %v), want one row", failed, err)
	}
	got := failed[0]
	if got.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed", got.ReconciliationStatus)
	}
	if got.LastReconciledAt == nil || !got.LastReconciledAt.Equal(readyAt) {
		t.Errorf("last_reconciled_at = %v, want unchanged %v", got.LastReconciledAt, readyAt)
	}
	if !got.UpdatedAt.After(readyUpdated) {
		t.Errorf("updated_at = %v, want advanced past %v", got.UpdatedAt, readyUpdated)
	}
}

func TestNetworkDeletePurgesNodeStatus(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	net := netParams(uniqueNetName("nns-del"))
	if _, err := s.CreateNetwork(ctx, net); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
			NetworkID: net.ID, NodeID: uuid.New(), ReconciliationStatus: "ready",
		}); err != nil {
			t.Fatalf("seed status %d: %v", i, err)
		}
	}
	if err := s.DeleteNetwork(ctx, net.ID); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	left, err := s.ListNetworkNodeStatusByNetwork(ctx, net.ID)
	if err != nil {
		t.Fatalf("ListNetworkNodeStatusByNetwork after delete: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("status records after delete = %d, want 0", len(left))
	}
}

// managedView projects the four managed-egress fields for cmp comparison.
// Subnet/Gateway are rendered to strings because netip.Prefix/netip.Addr carry
// unexported fields that go-cmp cannot compare without a custom option.
type managedView struct {
	Managed bool
	Egress  store.NetworkEgress
	Subnet  string
	Gateway string
}

func viewOf(n store.Network) managedView {
	v := managedView{Managed: n.Managed, Egress: n.Egress}
	if n.Subnet != nil {
		v.Subnet = n.Subnet.String()
	}
	if n.Gateway != nil {
		v.Gateway = n.Gateway.String()
	}
	return v
}

func idsOf(nets []store.Network) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.ID)
	}
	return out
}

// TestUpsertNetworkNodeStatusNoopIfUnchanged verifies the up-channel write
// guard: a steady-state heartbeat that re-reports the same (status, error) for a
// network must not rewrite the per-(node, network) record (updated_at frozen),
// while a changed report must write. Without the guard every heartbeat re-puts
// every network's status (N_nodes x N_networks PutJSON per interval).
func TestUpsertNetworkNodeStatusNoopIfUnchanged(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	node := nodeParams(uniqueNodeName("nnsnoop"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	net := netParams(uniqueNetName("nnsnoop"))
	if _, err := s.CreateNetwork(ctx, net); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	upsert := func(status string, errMsg *string) {
		t.Helper()
		if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
			NetworkID: net.ID, NodeID: node.ID, ReconciliationStatus: status, ReconciliationError: errMsg,
		}); err != nil {
			t.Fatalf("UpsertNetworkNodeStatus(%s): %v", status, err)
		}
	}
	updatedAt := func() time.Time {
		t.Helper()
		rows, err := s.ListNetworkNodeStatusByNetwork(ctx, net.ID)
		if err != nil {
			t.Fatalf("ListNetworkNodeStatusByNetwork: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		return rows[0].UpdatedAt
	}

	upsert("ready", nil)
	first := updatedAt()

	// Identical re-report: no-op, updated_at must stay byte-identical.
	time.Sleep(2 * time.Millisecond)
	upsert("ready", nil)
	if second := updatedAt(); !second.Equal(first) {
		t.Errorf("updated_at after identical re-report = %v, want unchanged %v (steady-state must not rewrite)", second, first)
	}

	// Changed report: must write, updated_at must advance.
	time.Sleep(2 * time.Millisecond)
	errMsg := "bridge create failed"
	upsert("failed", &errMsg)
	if third := updatedAt(); !third.After(first) {
		t.Errorf("updated_at after status change = %v, want after %v (real change must rewrite)", third, first)
	}
}

func TestCreateNetworkOverlayAllocatesVNI(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sn := netip.MustParsePrefix("10.50.0.0/24")
	n1, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-a", Type: store.NetworkTypeOverlay,
		Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("CreateNetwork ov-a: %v", err)
	}
	if n1.VNI == nil || *n1.VNI != 1000 {
		t.Errorf("ov-a VNI = %v, want 1000", n1.VNI)
	}
	if n1.BridgeName != "otvb1000" {
		t.Errorf("ov-a BridgeName = %q, want otvb1000", n1.BridgeName)
	}
	if !n1.Managed || n1.Egress != store.NetworkEgressNone || n1.Mtu != store.OverlayMTU {
		t.Errorf("ov-a forced fields = (managed=%v egress=%q mtu=%d), want (true none 1390)", n1.Managed, n1.Egress, n1.Mtu)
	}

	n2, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-b", Type: store.NetworkTypeOverlay,
		Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("CreateNetwork ov-b: %v", err)
	}
	if n2.VNI == nil || *n2.VNI != 1001 {
		t.Errorf("ov-b VNI = %v, want 1001", n2.VNI)
	}
}

func TestCreateNetworkPersistsDNSEnabled(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	pfx := netip.MustParsePrefix("10.77.0.0/24")
	got, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID:          uuid.New(),
		Name:        "dns-rt",
		Type:        store.NetworkTypeOverlay,
		Egress:      store.NetworkEgressNone,
		Subnet:      &pfx,
		DhcpEnabled: true,
		DNSEnabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if !got.DNSEnabled {
		t.Errorf("DNSEnabled = false, want true")
	}
}

func TestCreateOverlayStampsMtuFromUnderlay(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if err := s.SeedUnderlayMTU(ctx, 9000); err != nil {
		t.Fatalf("seed underlay: %v", err)
	}
	sn := netip.MustParsePrefix("10.50.0.0/24")
	n, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-jumbo", Type: store.NetworkTypeOverlay,
		Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create overlay: %v", err)
	}
	if n.Mtu != 9000-store.OverlayEncapOverhead {
		t.Errorf("overlay Mtu = %d, want underlay(9000) - 110 = 8890", n.Mtu)
	}
}

func TestCreateNetworkOverlayVNINoReclaimOnDelete(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	sn := netip.MustParsePrefix("10.50.0.0/24")

	n1, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-a", Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create ov-a: %v", err)
	}
	if err := s.DeleteNetwork(ctx, n1.ID); err != nil {
		t.Fatalf("delete ov-a: %v", err)
	}
	n2, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-c", Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create ov-c: %v", err)
	}
	if n2.VNI == nil || *n2.VNI != 1001 {
		t.Errorf("ov-c VNI = %v, want 1001 (no reclaim of 1000)", n2.VNI)
	}
}

func TestCreateNetworkOverlayVNIExhausted(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if err := s.SeedVNIRange(ctx, 1000, 1001); err != nil {
		t.Fatalf("seed range: %v", err)
	}
	sn := netip.MustParsePrefix("10.50.0.0/24")
	for i, name := range []string{"a", "b"} {
		if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
			ID: uuid.New(), Name: "ov-" + name, Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
		}); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	_, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-c", Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
	})
	if !errors.Is(err, store.ErrVNIExhausted) {
		t.Errorf("third overlay create err = %v, want ErrVNIExhausted", err)
	}
}

func TestCreateNetworkOverlayNameCollision(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	sn := netip.MustParsePrefix("10.50.0.0/24")
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "dup", Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "DUP", Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
	})
	if !errors.Is(err, store.ErrNetworkNameExists) {
		t.Errorf("dup create err = %v, want ErrNetworkNameExists", err)
	}
}

// TestDeleteNetworkLeavesReusedNameGuardIntact verifies that deleting network X
// does not clobber a name guard that was re-taken by a new network Y after X was
// deleted. The front-door path (DeleteNetwork rejects a soft-deleted row) already
// proves the guard condition is effective for the common serialised path; this
// test asserts the invariant-visible property: Y's name resolves to Y even after
// a stale delete attempt targeting X's id. This guards the end-state invariant;
// the Else branch itself is exercised directly by the internal-package
// TestDeleteNetworkElseBranchPreservesForeignGuard (this external test cannot
// reach the Else branch single-threaded - NetworkByID rejects the soft-deleted
// row at the front door before the txn runs).
func TestDeleteNetworkLeavesReusedNameGuardIntact(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	// Create X named "foo", capture its id.
	x, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "foo", Type: store.NetworkTypeBridge, BridgeName: "br0", Mtu: 1500, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create X: %v", err)
	}

	// Normal delete of X: guard "foo" still points to X -> Then branch -> full cleanup.
	if err := s.DeleteNetwork(ctx, x.ID); err != nil {
		t.Fatalf("delete X: %v", err)
	}
	if _, err := s.NetworkByName(ctx, "foo"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after delete X, NetworkByName(foo) err = %v, want ErrNotFound", err)
	}

	// Re-create Y reusing the freed name "foo".
	y, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "foo", Type: store.NetworkTypeBridge, BridgeName: "br1", Mtu: 1500, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create Y: %v", err)
	}

	// A stale delete targeting X.ID returns ErrNotFound at the front door (X is
	// already soft-deleted) - which itself proves the front-door guard. The
	// Else-branch protection is the belt for the genuinely-concurrent path.
	_ = s.DeleteNetwork(ctx, x.ID) // ErrNotFound expected; ignore

	// Y's name must still resolve to Y - the guard must not have been clobbered.
	got, err := s.NetworkByName(ctx, "foo")
	if err != nil {
		t.Fatalf("NetworkByName(foo) after stale delete attempt: %v", err)
	}
	if got.ID != y.ID {
		t.Errorf("name foo resolves to %s, want Y %s (guard clobbered)", got.ID, y.ID)
	}
}

// TestCreateNetworkOverlayStampsOverConflictingInput verifies that the store
// overwrites client-supplied server-owned fields for overlay networks. The
// handler rejects them at the API edge; the store is the last line of defense.
func TestCreateNetworkOverlayStampsOverConflictingInput(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	sn := netip.MustParsePrefix("10.50.0.0/24")
	got, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       "ov-stamp",
		Type:       store.NetworkTypeOverlay,
		Subnet:     &sn,
		Config:     []byte("{}"),
		BridgeName: "operator-bogus",       // must be overwritten with otvb<vni>
		Mtu:        1500,                   // must be overwritten with 1390
		Managed:    false,                  // must be forced true
		Egress:     store.NetworkEgressNAT, // operator-owned: preserved
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if got.VNI == nil {
		t.Fatalf("VNI not allocated")
	}
	wantBridge := "otvb" + strconv.Itoa(int(*got.VNI))
	if got.BridgeName != wantBridge || got.Mtu != store.OverlayMTU || !got.Managed {
		t.Errorf("forced fields = (bridge=%q mtu=%d managed=%v), want (%q 1390 true)",
			got.BridgeName, got.Mtu, got.Managed, wantBridge)
	}
	if got.Egress != store.NetworkEgressNAT {
		t.Errorf("Egress = %q, want %q (operator-owned, preserved)", got.Egress, store.NetworkEgressNAT)
	}
}
