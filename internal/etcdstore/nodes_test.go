// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

func nodeParams(name string) store.CreateNodeParams {
	return store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node.example:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	}
}

func uniqueNodeName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestNodeCreateGetByIDAndName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := nodeParams(uniqueNodeName("node"))
	created, err := s.CreateNode(ctx, p)
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if created.ID != p.ID || created.Status != store.NodeStatusPending || created.CreatedAt.IsZero() {
		t.Errorf("CreateNode = %+v, want id/pending/created_at", created)
	}
	byID, err := s.NodeByID(ctx, p.ID)
	if err != nil || byID.Name != p.Name {
		t.Fatalf("NodeByID = (%+v, %v)", byID, err)
	}
	byName, err := s.NodeByName(ctx, p.Name)
	if err != nil || byName.ID != p.ID {
		t.Fatalf("NodeByName = (%+v, %v)", byName, err)
	}
}

func TestCreateNodePersistsIngressAdvertisedEndpoint(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	const want = "https://gw-1.example:9444"
	p := store.CreateNodeParams{
		ID:                        uuid.New(),
		Name:                      uniqueNodeName("gw"),
		Gateway:                   true,
		Architecture:              store.CpuArchArm64,
		AdvertisedEndpoint:        "https://gw-1.example:9443",
		IngressAdvertisedEndpoint: want,
		MigrationHost:             "10.0.0.1",
		MigrationPortRangeStart:   49152,
		MigrationPortRangeEnd:     49251,
		Status:                    store.NodeStatusPending,
	}
	if _, err := s.CreateNode(ctx, p); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	got, err := s.NodeByName(ctx, p.Name)
	if err != nil {
		t.Fatalf("NodeByName: %v", err)
	}
	if got.IngressAdvertisedEndpoint != want {
		t.Errorf("IngressAdvertisedEndpoint = %q, want %q", got.IngressAdvertisedEndpoint, want)
	}
}

func TestNodeNameUniquenessAndNotFound(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueNodeName("dup")
	if _, err := s.CreateNode(ctx, nodeParams(name)); err != nil {
		t.Fatalf("first CreateNode: %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeParams(name)); !errors.Is(err, store.ErrNodeNameExists) {
		t.Errorf("dup node name = %v, want store.ErrNodeNameExists", err)
	}
	if _, err := s.NodeByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeByID(absent) = %v, want store.ErrNotFound", err)
	}
	if _, err := s.NodeByName(ctx, uniqueNodeName("missing")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeByName(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestNodeCordonUncordon(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := nodeParams(uniqueNodeName("cordon"))
	if _, err := s.CreateNode(ctx, p); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	cordoned, err := s.CordonNode(ctx, p.ID)
	if err != nil {
		t.Fatalf("CordonNode: %v", err)
	}
	if cordoned.Status != store.NodeStatusCordoned || cordoned.CordonedAt == nil {
		t.Errorf("CordonNode = %+v, want cordoned + cordoned_at set", cordoned)
	}
	uncordoned, err := s.UncordonNode(ctx, p.ID)
	if err != nil {
		t.Fatalf("UncordonNode: %v", err)
	}
	if uncordoned.Status != store.NodeStatusReady || uncordoned.CordonedAt != nil {
		t.Errorf("UncordonNode = %+v, want ready + cordoned_at nil", uncordoned)
	}
}

func TestReadmitNode(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	// gone -> pending
	goneID := seedNodeRow(t, cli, "gone", store.NodeStatusGone, nil, nil)
	got, err := s.ReadmitNode(ctx, goneID)
	if err != nil {
		t.Fatalf("ReadmitNode(gone) error = %v, want nil", err)
	}
	if got.Status != store.NodeStatusPending {
		t.Errorf("ReadmitNode(gone).Status = %v, want %v", got.Status, store.NodeStatusPending)
	}
	reread, err := s.NodeByID(ctx, goneID)
	if err != nil {
		t.Fatalf("NodeByID after readmit: %v", err)
	}
	if reread.Status != store.NodeStatusPending {
		t.Errorf("persisted status = %v, want pending", reread.Status)
	}

	// non-gone source states are refused with ErrConcurrentUpdate
	for _, status := range []store.NodeStatus{
		store.NodeStatusReady, store.NodeStatusPending,
		store.NodeStatusCordoned, store.NodeStatusUnreachable,
	} {
		id := seedNodeRow(t, cli, "n-"+string(status), status, nil, nil)
		if _, err := s.ReadmitNode(ctx, id); !errors.Is(err, store.ErrConcurrentUpdate) {
			t.Errorf("ReadmitNode(%s) error = %v, want ErrConcurrentUpdate", status, err)
		}
	}

	// missing node -> ErrNotFound
	if _, err := s.ReadmitNode(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ReadmitNode(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestNodeListEffectiveFilterAndPagination(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		p := nodeParams(uniqueNodeName("list"))
		if _, err := s.CreateNode(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, p.ID)
		time.Sleep(2 * time.Millisecond)
	}
	all, err := s.ListNodesEffective(ctx, store.ListNodesEffectiveParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListNodesEffective: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("ListNodesEffective len = %d, want >= 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Errorf("not ascending at %d", i)
		}
	}
	arch := store.CpuArchArm64
	none, err := s.ListNodesEffective(ctx, store.ListNodesEffectiveParams{Architecture: &arch, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListNodesEffective(arm): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ListNodesEffective(arm64) = %d, want 0", len(none))
	}
	page, err := s.ListNodesEffective(ctx, store.ListNodesEffectiveParams{
		CursorCreatedAt: &all[0].CreatedAt, CursorID: &all[0].ID, LimitCount: 1,
	})
	if err != nil {
		t.Fatalf("ListNodesEffective paged: %v", err)
	}
	if len(page) != 1 || page[0].ID != all[1].ID {
		t.Errorf("paged = %v, want [%v]", page, all[1].ID)
	}
}

func TestNodeEffectivePendingReservations(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	// Seed a node row directly with reported availability and a heartbeat in the
	// past, so a later-created pinned VM counts as pending.
	hb := time.Now().UTC().Add(-time.Hour)
	cpuAvail := int32(16)
	memAvail := int64(32768)
	nodeID := uuid.New()
	n := store.Node{
		ID:                 nodeID,
		Name:               uniqueNodeName("eff"),
		Architecture:       store.CpuArchAmd64,
		AdvertisedEndpoint: "https://eff.example:9443",
		MigrationHost:      "10.0.0.2",
		Status:             store.NodeStatusReady,
		CPUCoresAvailable:  &cpuAvail,
		MemoryAvailableMib: &memAvail,
		LastHeartbeatAt:    &hb,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("nodes", nodeID.String()), n); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	// Pinned VM created after the heartbeat -> pending.
	vmID := uuid.New()
	vm := store.VM{ID: vmID, Name: "vm-pending", DesiredPhase: store.VmDesiredPhaseRunning, PinnedNodeID: &nodeID, CpuCores: 4, MemoryMib: 8192, CreatedAt: time.Now().UTC()}
	if err := cli.PutJSON(ctx, etcd.Key("vms", vmID.String()), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vms", "pinned_node", nodeID.String(), vmID.String()), []byte(vmID.String())); err != nil {
		t.Fatalf("seed pinned index: %v", err)
	}

	eff, err := s.NodeEffectiveByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("NodeEffectiveByID: %v", err)
	}
	if eff.CPUCoresEffective == nil || *eff.CPUCoresEffective != 12 {
		t.Errorf("CPUCoresEffective = %v, want 12 (16-4)", eff.CPUCoresEffective)
	}
	if eff.MemoryEffectiveMib == nil || *eff.MemoryEffectiveMib != 24576 {
		t.Errorf("MemoryEffectiveMib = %v, want 24576 (32768-8192)", eff.MemoryEffectiveMib)
	}
}

func TestNodeDeleteBlockingAndForceCascade(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := nodeParams(uniqueNodeName("del"))
	if _, err := s.CreateNode(ctx, p); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	// Seed a vm_runtime on the node + an active migration touching it.
	vmID := uuid.New()
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &p.ID, Phase: store.VmPhaseRunning}
	if err := cli.PutJSON(ctx, etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("seed vm_runtime: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_runtime", "node", p.ID.String(), vmID.String()), []byte(vmID.String())); err != nil {
		t.Fatalf("seed vm_runtime index: %v", err)
	}
	migID := uuid.New()
	mig := store.Migration{ID: migID, VmID: vmID, SourceNodeID: &p.ID, Phase: store.MigrationPhaseActive}
	if err := cli.PutJSON(ctx, etcd.Key("migrations", migID.String()), mig); err != nil {
		t.Fatalf("seed migration: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "migrations", "node", p.ID.String(), migID.String()), []byte(migID.String())); err != nil {
		t.Fatalf("seed migration index: %v", err)
	}

	// Non-force delete is blocked.
	var blocking *store.ResourceInUseError
	if _, err := s.DeleteNode(ctx, p.ID, false, uuid.New()); !errors.As(err, &blocking) {
		t.Fatalf("DeleteNode(non-force) = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vms"] != 1 || blocking.Resources["active_migrations"] != 1 {
		t.Errorf("blocking = %v, want vms=1 active_migrations=1", blocking.Resources)
	}

	// Force delete cascades.
	out, err := s.DeleteNode(ctx, p.ID, true, uuid.New())
	if err != nil {
		t.Fatalf("DeleteNode(force): %v", err)
	}
	if out.VMsOrphaned != 1 || out.MigrationsCancelled != 1 {
		t.Errorf("outcome = %+v, want VMsOrphaned=1 MigrationsCancelled=1", out)
	}
	if _, err := s.NodeByID(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeByID after force-delete = %v, want store.ErrNotFound", err)
	}
	// vm_runtime orphaned, migration cancelled.
	var gotRT store.VMRuntime
	if _, err := cli.GetJSON(ctx, etcd.Key("vm_runtime", vmID.String()), &gotRT); err != nil {
		t.Fatalf("read vm_runtime: %v", err)
	}
	if gotRT.Phase != store.VmPhaseOrphaned || gotRT.CurrentNodeID != nil {
		t.Errorf("vm_runtime = %+v, want orphaned + nil node", gotRT)
	}
	var gotMig store.Migration
	if _, err := cli.GetJSON(ctx, etcd.Key("migrations", migID.String()), &gotMig); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if gotMig.Phase != store.MigrationPhaseCancelled || gotMig.ErrorMessage == nil {
		t.Errorf("migration = %+v, want cancelled + error_message", gotMig)
	}
	// Node name reusable after delete.
	if _, err := s.CreateNode(ctx, nodeParams(p.Name)); err != nil {
		t.Errorf("node name not reusable after delete: %v", err)
	}
}

// TestNodeForceDeleteRetryConvergence drives the real DeleteNode force path twice
// to prove the chunked cascade converges on retry: the first call cancels +
// orphans + soft-deletes; the second sees the soft-deleted node row and returns
// store.ErrNotFound without double-cancel/double-orphan corruption.
func TestNodeForceDeleteRetryConvergence(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := nodeParams(uniqueNodeName("conv"))
	if _, err := s.CreateNode(ctx, p); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	vmID := uuid.New()
	rt := store.VMRuntime{VmID: vmID, CurrentNodeID: &p.ID, Phase: store.VmPhaseRunning}
	if err := cli.PutJSON(ctx, etcd.Key("vm_runtime", vmID.String()), rt); err != nil {
		t.Fatalf("seed vm_runtime: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_runtime", "node", p.ID.String(), vmID.String()), []byte(vmID.String())); err != nil {
		t.Fatalf("seed vm_runtime index: %v", err)
	}
	migID := uuid.New()
	mig := store.Migration{ID: migID, VmID: vmID, SourceNodeID: &p.ID, Phase: store.MigrationPhaseActive}
	if err := cli.PutJSON(ctx, etcd.Key("migrations", migID.String()), mig); err != nil {
		t.Fatalf("seed migration: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "migrations", "node", p.ID.String(), migID.String()), []byte(migID.String())); err != nil {
		t.Fatalf("seed migration index: %v", err)
	}

	out, err := s.DeleteNode(ctx, p.ID, true, uuid.New())
	if err != nil {
		t.Fatalf("DeleteNode(force) first call: %v", err)
	}
	if out.VMsOrphaned != 1 || out.MigrationsCancelled != 1 {
		t.Errorf("outcome = %+v, want VMsOrphaned=1 MigrationsCancelled=1", out)
	}

	var gotRT store.VMRuntime
	if _, err := cli.GetJSON(ctx, etcd.Key("vm_runtime", vmID.String()), &gotRT); err != nil {
		t.Fatalf("read vm_runtime: %v", err)
	}
	if gotRT.Phase != store.VmPhaseOrphaned || gotRT.CurrentNodeID != nil {
		t.Errorf("vm_runtime = %+v, want orphaned + nil node", gotRT)
	}
	var gotMig store.Migration
	if _, err := cli.GetJSON(ctx, etcd.Key("migrations", migID.String()), &gotMig); err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if gotMig.Phase != store.MigrationPhaseCancelled {
		t.Errorf("migration phase = %v, want cancelled", gotMig.Phase)
	}

	// Retry: node row is already soft-deleted, so NodeByID at the top returns
	// ErrNotFound - convergence with no double-cancel/double-orphan.
	if _, err := s.DeleteNode(ctx, p.ID, true, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteNode(force) retry = %v, want store.ErrNotFound", err)
	}
}

func TestSetNodeGatewayRole(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	// A freshly created node defaults to the hypervisor role (GatewayRole false).
	created, err := s.CreateNode(ctx, nodeParams(uniqueNodeName("node")))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	got, err := s.SetNodeGatewayRole(ctx, created.ID, true)
	if err != nil {
		t.Fatalf("SetNodeGatewayRole(true) error = %v", err)
	}
	if !got.HasRole(store.NodeRoleGateway) {
		t.Errorf("after enable HasRole(gateway) = false, want true")
	}

	// Idempotent: enabling again is a no-op that still returns the row.
	again, err := s.SetNodeGatewayRole(ctx, created.ID, true)
	if err != nil {
		t.Fatalf("SetNodeGatewayRole(true) second call error = %v", err)
	}
	if !again.HasRole(store.NodeRoleGateway) {
		t.Errorf("idempotent enable lost the role")
	}

	off, err := s.SetNodeGatewayRole(ctx, created.ID, false)
	if err != nil {
		t.Fatalf("SetNodeGatewayRole(false) error = %v", err)
	}
	if off.HasRole(store.NodeRoleGateway) {
		t.Errorf("after disable HasRole(gateway) = true, want false")
	}

	// A missing node resolves to store.ErrNotFound.
	if _, err := s.SetNodeGatewayRole(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetNodeGatewayRole(missing) = %v, want store.ErrNotFound", err)
	}
}
