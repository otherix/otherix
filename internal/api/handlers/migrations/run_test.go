// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/migrations"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustParseMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

func mustParseAddr(t *testing.T, s string) *netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return &a
}

// TestMigrationNicsFromVM proves each (vm_nic, network) pair maps to a
// MigrationNic carrying bridge / mac / model / mtu / device_order, with ipv4
// included only when the NIC has a static address. Bridge name and MTU come
// from the resolved network (the vm_nic row does not carry them).
func TestMigrationNicsFromVM(t *testing.T) {
	vmID := uuid.New()
	pairs := []migrations.NicNetworkPair{
		{
			Nic: store.VMNic{
				ID:          uuid.New(),
				VmID:        vmID,
				MacAddress:  mustParseMAC(t, "52:54:00:00:00:01"),
				Model:       store.NicModelVirtio,
				DeviceOrder: 0,
				Ipv4Address: mustParseAddr(t, "10.42.0.5"),
			},
			Network: store.Network{BridgeName: "otvb100", Mtu: 1390},
		},
		{
			Nic: store.VMNic{
				ID:          uuid.New(),
				VmID:        vmID,
				MacAddress:  mustParseMAC(t, "52:54:00:00:00:02"),
				Model:       store.NicModelVirtio,
				DeviceOrder: 1,
			},
			Network: store.Network{BridgeName: "vmbr0", Mtu: 1500},
		},
	}

	got := migrations.MigrationNics(pairs)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].BridgeName != "otvb100" || got[0].MacAddress != "52:54:00:00:00:01" ||
		got[0].Model != agentapi.MigrationNicModelVirtio || got[0].DeviceOrder != 0 {
		t.Errorf("nic0 = %+v, want bridge otvb100 mac ...01 virtio order 0", got[0])
	}
	if got[0].Mtu == nil || *got[0].Mtu != 1390 {
		t.Errorf("nic0 mtu = %v, want 1390", got[0].Mtu)
	}
	if got[0].Ipv4Address == nil || *got[0].Ipv4Address != "10.42.0.5" {
		t.Errorf("nic0 ipv4 = %v, want 10.42.0.5", got[0].Ipv4Address)
	}
	if got[1].BridgeName != "vmbr0" || got[1].Mtu == nil || *got[1].Mtu != 1500 {
		t.Errorf("nic1 = %+v, want bridge vmbr0 mtu 1500", got[1])
	}
	if got[1].Ipv4Address != nil {
		t.Errorf("nic1 ipv4 = %v, want nil", got[1].Ipv4Address)
	}
}

// fakeMigrationAgent is the agent seam double. It records every call so the
// pending path can prove the agent was NOT contacted, and replays a staged
// outgoing-task terminal so the success / failure branches converge
// synchronously (no real polling).
type fakeMigrationAgent struct {
	mu sync.Mutex

	incomingCalls int
	outgoingCalls int
	pollCalls     int

	startTargetCalls  int
	deleteSourceCalls int

	// nudged records every endpoint NudgeHeartbeat was called with; the
	// worker fast-pushes to the computed overlay peer set.
	nudged []string

	// cancelCalls records every CancelMigration the worker issues to a bound
	// target (live-migration incoming teardown on a pre-cutover terminal).
	cancelCalls []cancelCall

	// incomingReq / outgoingReq capture the last requests so a test can assert the
	// threaded identities + disk spec.
	incomingReq agentapi.MigrationIncomingRequest
	outgoingReq agentapi.MigrationOutgoingRequest

	// terminal is the staged outgoing-task outcome the worker's PollTask sees.
	terminal agentclient.TaskTerminal
	pollErr  error

	// pollHook, when set, runs inside PollTask (before it returns terminal) so a
	// test can inject a mid-flight state change between drive-start and the
	// source-success cutover - e.g. a too-late operator cancel that races the
	// auto-resume of the target. Runs OUTSIDE the agent's lock.
	pollHook func()

	// startTargetErr / deleteSourceErr stage post-cutover convergence failures so a
	// test can prove the worker logs-and-continues (never fails the committed migration).
	startTargetErr  error
	deleteSourceErr error

	// incomingNbdEndpoint, when set, is returned as the StartIncomingMigration
	// response's NbdEndpoint so a test can assert the worker relays it onto the
	// outgoing request (it lands in outgoingReq.NbdEndpoint).
	incomingNbdEndpoint *string

	// agentTaskID is the id StartOutgoingMigration returns (and PollTask echoes).
	agentTaskID uuid.UUID

	// sourceVM is what VM(...) returns - the worker reads its
	// BootDiskVirtualSizeBytes to size the destination disk. Default to a sane
	// non-zero size so existing tests clear the worker's zero-size guard.
	sourceVM agentclient.AgentVM
}

func (f *fakeMigrationAgent) StartIncomingMigration(_ context.Context, _, _ string, req agentapi.MigrationIncomingRequest) (agentapi.MigrationIncomingResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incomingCalls++
	f.incomingReq = req
	return agentapi.MigrationIncomingResponse{
		AuthToken:      "tok-" + req.MigrationID.String(),
		ListenEndpoint: "10.0.0.2:49152",
		MigrationID:    req.MigrationID,
		NbdEndpoint:    f.incomingNbdEndpoint,
	}, nil
}

func (f *fakeMigrationAgent) StartOutgoingMigration(_ context.Context, _, _ string, req agentapi.MigrationOutgoingRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outgoingCalls++
	f.outgoingReq = req
	if f.agentTaskID == uuid.Nil {
		f.agentTaskID = uuid.New()
	}
	return f.agentTaskID.String(), nil
}

func (f *fakeMigrationAgent) PollTask(_ context.Context, _ string, _ uuid.UUID) (agentclient.TaskTerminal, error) {
	f.mu.Lock()
	f.pollCalls++
	pollErr := f.pollErr
	hook := f.pollHook
	terminal := f.terminal
	f.mu.Unlock()
	if pollErr != nil {
		return agentclient.TaskTerminal{}, pollErr
	}
	if hook != nil {
		hook()
	}
	return terminal, nil
}

func (f *fakeMigrationAgent) StartVMOnTarget(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startTargetCalls++
	return f.startTargetErr
}

func (f *fakeMigrationAgent) DeleteVMOnSource(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteSourceCalls++
	return f.deleteSourceErr
}

func (f *fakeMigrationAgent) VM(_ context.Context, _, _ string) (agentclient.AgentVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := f.sourceVM
	if vm.BootDiskVirtualSizeBytes == 0 {
		// Default to a sane non-zero size so tests that don't care about disk
		// sizing clear the worker's zero-size guard.
		vm.BootDiskVirtualSizeBytes = 10 * 1024 * 1024 * 1024
	}
	return vm, nil
}

func (f *fakeMigrationAgent) NudgeHeartbeat(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nudged = append(f.nudged, endpoint)
	return nil
}

// cancelCall records the arguments of one CancelMigration invocation.
type cancelCall struct{ endpoint, vmName, migID string }

func (f *fakeMigrationAgent) CancelMigration(_ context.Context, endpoint, vmName, migrationID string) (agentapi.Migration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, cancelCall{endpoint, vmName, migrationID})
	return agentapi.Migration{}, nil
}

// fakePlacer is the placement seam double. It returns a fixed decision or a
// staged error (e.g. ErrNoEligibleNodes for the single-node pending path).
type fakePlacer struct {
	decision scheduler.PlacementDecision
	err      error
	calls    int
}

func (p *fakePlacer) Place(context.Context, scheduler.PlacementRequest) (scheduler.PlacementDecision, error) {
	p.calls++
	if p.err != nil {
		return scheduler.PlacementDecision{}, p.err
	}
	return p.decision, nil
}

// seedReadyNode creates a node row in the ready state with a distinct advertised
// endpoint so the worker can resolve source/target endpoints.
func seedReadyNode(t *testing.T, s *etcdstore.Store, name, endpoint string) store.Node {
	t.Helper()
	ctx := context.Background()
	n, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      endpoint,
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode(%s): %v", name, err)
	}
	return n
}

// seedPinnedVM writes a vms row pinned to node via the etcd client (the worker
// reads it through VMByID; cutover re-pins it).
func seedPinnedVM(t *testing.T, cli *etcd.Client, node uuid.UUID) store.VM {
	t.Helper()
	ctx := context.Background()
	pin := node
	v := store.VM{
		ID: uuid.New(), OwnerID: uuid.New(), Name: "mig-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		CpuCores: 2, MemoryMib: 2048, PinnedNodeID: &pin,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("vms", v.ID.String()), v); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", v.Name), []byte(v.ID.String())); err != nil {
		t.Fatalf("seed vm guard: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vms", "pinned_node", node.String(), v.ID.String()), []byte(v.ID.String())); err != nil {
		t.Fatalf("seed pinned_node index: %v", err)
	}
	return v
}

// seedPool creates a storage pool named name on node with the given filesystem
// path, so the worker can resolve TargetPoolName -> StoragePoolPath on the
// target node.
func seedPool(t *testing.T, s *etcdstore.Store, node uuid.UUID, name, path string) store.StoragePool {
	t.Helper()
	p, err := s.CreateStoragePool(context.Background(), store.CreateStoragePoolParams{
		ID: uuid.New(), NodeID: node, Name: name, Type: "local_dir", Path: path,
	})
	if err != nil {
		t.Fatalf("CreateStoragePool(%s on %s): %v", name, node, err)
	}
	return p
}

// migrationJobArgsStub is the minimal queue.JobArgs the enqueue path marshals.
type migrationJobArgsStub struct {
	TaskID      uuid.UUID
	MigrationID uuid.UUID
}

func (migrationJobArgsStub) Kind() string { return "vm.migrate" }

// seedNodelessMigration creates a node-less (target nil) migration + backing task
// for vm pinned to source, returning the migration row and the backing task id.
func seedNodelessMigration(t *testing.T, s *etcdstore.Store, vmID, source uuid.UUID, live bool) (store.Migration, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	src := source
	taskID := uuid.New()
	migID := uuid.New()
	resID := migID
	p := store.CreateMigrationParams{
		ID:           migID,
		VmID:         vmID,
		SourceNodeID: &src,
		TargetNodeID: nil,
		Reason:       store.MigrationReasonManual,
		Live:         live,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.migrate", Status: store.TaskStatusPending,
			ResourceType: "migration", ResourceID: &resID, MaxAttempts: 25,
		},
	}
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: taskID, MigrationID: migID})
	if err != nil {
		t.Fatalf("CreateMigration(nodeless): %v", err)
	}
	return m, taskID
}

// seedExplicitMigration creates an explicit-target (--node) migration + backing
// task for vm: TargetNodeID is pre-bound to target, with the given pool name
// (pass "" to exercise the empty-pool default-to-source path). The migration is
// NOT node-less, so the worker skips placeAndBind and goes straight to the
// handshake.
func seedExplicitMigration(t *testing.T, s *etcdstore.Store, vmID, source, target uuid.UUID, pool string, live bool) (store.Migration, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	src := source
	tgt := target
	taskID := uuid.New()
	migID := uuid.New()
	resID := migID
	p := store.CreateMigrationParams{
		ID:             migID,
		VmID:           vmID,
		SourceNodeID:   &src,
		TargetNodeID:   &tgt,
		TargetPoolName: pool,
		Reason:         store.MigrationReasonManual,
		Live:           live,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.migrate", Status: store.TaskStatusPending,
			ResourceType: "migration", ResourceID: &resID, MaxAttempts: 25,
		},
	}
	m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: taskID, MigrationID: migID})
	if err != nil {
		t.Fatalf("CreateMigration(explicit): %v", err)
	}
	return m, taskID
}

// seedBootDiskInPool writes a device-order-0 boot disk for vm whose
// StoragePoolID points at poolID, so sourcePoolName resolves the boot disk to a
// pool with a known name.
func seedBootDiskInPool(t *testing.T, cli *etcd.Client, vmID, poolID uuid.UUID, sizeGib int32) store.VMDisk {
	t.Helper()
	ctx := context.Background()
	d := store.VMDisk{
		ID: uuid.New(), VmID: vmID, StoragePoolID: poolID,
		DeviceOrder: 0, Bus: store.DiskBusVirtio, SizeGib: sizeGib,
		SourceKind: "image", Format: store.ImageFormatQcow2,
		CacheMode: store.DiskCacheModeWriteback, Discard: store.DiskDiscardUnmap,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("vm_disks", d.ID.String()), d); err != nil {
		t.Fatalf("seed vm disk: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_disks", "vm", vmID.String(), d.ID.String()), []byte(d.ID.String())); err != nil {
		t.Fatalf("seed vm disk index: %v", err)
	}
	return d
}

// seedBridgeNetwork creates a cluster-wide bridge network row so a NIC can
// reference it and the placement network-readiness filter can be exercised.
func seedBridgeNetwork(t *testing.T, s *etcdstore.Store, name, bridge string) store.Network {
	t.Helper()
	n, err := s.CreateNetwork(context.Background(), store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       name,
		Type:       store.NetworkTypeBridge,
		BridgeName: bridge,
		Mtu:        1500,
	})
	if err != nil {
		t.Fatalf("CreateNetwork(%s): %v", name, err)
	}
	return n
}

// seedVMNic writes a device-order-0 NIC for vm on networkID via the raw client
// (mirroring the vm_nic row + per-vm index ListVMNicsByVM reads), so
// placeAndBind's ListVMNicsByVM resolves the VM's network ids.
func seedVMNic(t *testing.T, cli *etcd.Client, vmID, networkID uuid.UUID, mac string) store.VMNic {
	t.Helper()
	ctx := context.Background()
	n := store.VMNic{
		ID: uuid.New(), VmID: vmID, NetworkID: networkID,
		DeviceOrder: 0, Model: store.NicModelVirtio,
		MacAddress: mustParseMAC(t, mac),
		CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := cli.PutJSON(ctx, etcd.Key("vm_nics", n.ID.String()), n); err != nil {
		t.Fatalf("seed vm nic: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_nics", "vm", vmID.String(), n.ID.String()), []byte(n.ID.String())); err != nil {
		t.Fatalf("seed vm nic index: %v", err)
	}
	return n
}

// TestRunMigration_DefaultsTargetPoolToSourcePool pins correction A: an explicit
// --node migration with an EMPTY TargetPoolName defaults the target pool to the
// SOURCE VM's pool NAME (not the cluster default). The source boot disk lives in
// pool "src-pool"; the target node has its own "src-pool" instance with a
// distinct path. The worker must resolve the source pool name on the TARGET node,
// so the StartIncoming VMSpec disk carries the TARGET's "src-pool" path.
func TestRunMigration_DefaultsTargetPoolToSourcePool(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)

	// Source pool instance on node A; the boot disk lives in it.
	srcPool := seedPool(t, s, nodeA.ID, "src-pool", "/var/lib/otherix/pools/src-pool-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	// Target node B has its OWN "src-pool" instance with a DISTINCT path.
	const targetPath = "/var/lib/otherix/pools/src-pool-on-b"
	seedPool(t, s, nodeB.ID, "src-pool", targetPath)
	// A cluster "default" pool also exists on B - the worker must NOT pick it.
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default-on-b")

	// Explicit --node migration to B with NO pool (empty TargetPoolName).
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "", true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{} // must NOT be called: the migration is already bound.

	// DefaultPoolName set to a SENTINEL the worker must NOT use - correction A
	// requires the source pool name, never the cluster default.
	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{DefaultPoolName: "default"}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(default-pool) = %v, want nil", err)
	}

	if placer.calls != 0 {
		t.Errorf("placer called %d times on an explicit-node migration, want 0", placer.calls)
	}
	if len(agent.incomingReq.VMSpec.Disks) != 1 {
		t.Fatalf("incoming VMSpec.Disks len = %d, want 1", len(agent.incomingReq.VMSpec.Disks))
	}
	got := agent.incomingReq.VMSpec.Disks[0].StoragePoolPath
	if got != targetPath {
		t.Errorf("StoragePoolPath = %q, want target node's src-pool path %q (source pool name resolved on target, NOT cluster default)", got, targetPath)
	}

	// The cutover still committed (success path).
	gotMig, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if gotMig.Phase != store.MigrationPhaseCompleted {
		t.Errorf("migration phase = %q, want completed", gotMig.Phase)
	}
}

// TestRunMigration_PendingWhenTargetPoolAbsent pins correction B: an explicit
// --node migration whose target node does NOT have the (source) pool goes to
// PENDING (retryable, D4), NOT failed. The worker records phase=pending +
// scheduling_reason=pool_not_ready, returns a retryable error, leaves the VM on
// source, and NEVER contacts the agent (no StartIncoming, no DeleteVMOnSource).
func TestRunMigration_PendingWhenTargetPoolAbsent(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)

	// Source pool on node A; boot disk lives in it. Node B has NO "src-pool".
	srcPool := seedPool(t, s, nodeA.ID, "src-pool", "/var/lib/otherix/pools/src-pool-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)

	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "", true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	err := h(ctx, jobArgs(t, taskID, m.ID))
	if err == nil {
		t.Fatalf("MigrateHandler(pool-absent) = nil, want a retryable error so the dispatcher requeues")
	}

	got, err2 := s.MigrationByID(ctx, m.ID)
	if err2 != nil {
		t.Fatalf("MigrationByID: %v", err2)
	}
	if got.Phase != store.MigrationPhasePending {
		t.Errorf("migration phase = %q, want pending", got.Phase)
	}
	if got.SchedulingReason == nil || *got.SchedulingReason != migrations.ReasonPoolNotReady {
		t.Errorf("scheduling_reason = %v, want %q", got.SchedulingReason, migrations.ReasonPoolNotReady)
	}
	// The agent was NEVER contacted, and source was NOT deleted (no durable move).
	if agent.incomingCalls != 0 || agent.outgoingCalls != 0 || agent.pollCalls != 0 {
		t.Errorf("agent contacted on the pool-absent pending path: incoming=%d outgoing=%d poll=%d, want 0/0/0",
			agent.incomingCalls, agent.outgoingCalls, agent.pollCalls)
	}
	if agent.deleteSourceCalls != 0 {
		t.Errorf("DeleteVMOnSource calls = %d, want 0 (pending - VM stays on source)", agent.deleteSourceCalls)
	}
	// PinnedNodeID unchanged: the VM is still on its source.
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v (pending, no cutover)", gotVM.PinnedNodeID, nodeA.ID)
	}
}

// TestRunMigration_HappyNodeless drives the full node-less success path: the
// placer picks target B, the worker binds it, runs the two-phase handshake, the
// source outgoing task converges, and CommitMigrationCutover re-pins the VM to B.
// The migration is OFFLINE (live=false) so the post-cutover StartVMOnTarget runs
// (the live path self-resumes and is covered by
// TestDriveHandshake_LiveSkipsStartVMOnTarget).
func TestRunMigration_HappyNodeless(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	// Source pool on A so the empty-pool path resolves the source pool NAME; the
	// target node B has the same name so the bound target's pool is found.
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, false)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(happy) = %v, want nil", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCompleted {
		t.Errorf("migration phase = %q, want completed", got.Phase)
	}
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want target %v (cutover must re-pin)", gotVM.PinnedNodeID, nodeB.ID)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task status = %q, want success", task.Status)
	}
	if agent.incomingCalls != 1 || agent.outgoingCalls != 1 {
		t.Errorf("agent calls: incoming=%d outgoing=%d, want 1/1", agent.incomingCalls, agent.outgoingCalls)
	}

	// Peer identity threading: the incoming request pins the SOURCE node's
	// leaf-cert DN; the outgoing request pins the TARGET node's SAN name. The
	// builders prepend "node-" to the node NAME, so a node named "node-a" yields
	// "CN=node-node-a", and "node-b" yields "node-node-b.agents.otherix.local".
	if got := derefStr(agent.incomingReq.SourceNodeIdentity); got != "CN=node-"+nodeA.Name {
		t.Errorf("incoming SourceNodeIdentity = %q, want %q", got, "CN=node-"+nodeA.Name)
	}
	if got := derefStr(agent.outgoingReq.TargetNodeIdentity); got != "node-"+nodeB.Name+".agents.otherix.local" {
		t.Errorf("outgoing TargetNodeIdentity = %q, want %q", got, "node-"+nodeB.Name+".agents.otherix.local")
	}

	// VMSpec boot disk: the agent sizes the destination disk from Disks[0].SizeGib
	// and resolves the target pool from a non-empty Disks[0].StoragePoolPath.
	if len(agent.incomingReq.VMSpec.Disks) != 1 {
		t.Fatalf("incoming VMSpec.Disks len = %d, want 1", len(agent.incomingReq.VMSpec.Disks))
	}
	boot := agent.incomingReq.VMSpec.Disks[0]
	if boot.SizeGib != 40 {
		t.Errorf("incoming VMSpec.Disks[0].SizeGib = %d, want 40", boot.SizeGib)
	}
	if boot.StoragePoolPath == "" {
		t.Errorf("incoming VMSpec.Disks[0].StoragePoolPath = empty, want the target pool root path")
	}

	// Post-cutover convergence: the VM desires running, so the worker starts it on
	// the target, then deletes the source's stale copy (both reached ONLY after
	// the committed cutover).
	if agent.startTargetCalls != 1 {
		t.Errorf("StartVMOnTarget calls = %d, want 1 (desired running)", agent.startTargetCalls)
	}
	if agent.deleteSourceCalls != 1 {
		t.Errorf("DeleteVMOnSource calls = %d, want 1 (post-cutover cleanup)", agent.deleteSourceCalls)
	}
}

// TestRunMigration_SourceSuccessOverridesRacedCancel proves the
// auto-complete-vs-cancel race fix at the WORKER seam (the real drive sequence).
// A live migration's target QEMU auto-resumes the guest the instant the RAM
// stream completes - autonomously, before the CP commits the cutover. If an
// operator `migration cancel` lands in that window (here injected via pollHook,
// firing right before the source reports SUCCESS), the migration is marked
// cancelled. The source-success cutover MUST override the cancel and complete to
// the target, otherwise the VM is left RUNNING on the target while the CP records
// cancelled (split-brain risk). Assert: migration -> completed, VM re-pinned to
// the target, the source's stale copy deleted (post-cutover convergence ran).
func TestRunMigration_SourceSuccessOverridesRacedCancel(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	// The too-late cancel: it lands while the target is already (auto-)resuming,
	// just before the source push reports success. After this, the migration is
	// terminal-cancelled - but the source has already handed off.
	agent.pollHook = func() {
		if _, err := s.CancelMigration(ctx, m.ID, "operator_cancel"); err != nil {
			t.Errorf("CancelMigration(raced): %v", err)
		}
	}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(raced-cancel) = %v, want nil (cutover overrides the too-late cancel)", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCompleted {
		t.Errorf("migration phase = %q, want completed (cutover overrides the raced cancel)", got.Phase)
	}
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want target %v (no orphaned running target)", gotVM.PinnedNodeID, nodeB.ID)
	}
	// Post-cutover convergence ran: the source's stale copy is deleted.
	if agent.deleteSourceCalls != 1 {
		t.Errorf("DeleteVMOnSource calls = %d, want 1 (committed cutover converges)", agent.deleteSourceCalls)
	}
}

// TestDriveHandshake_LiveSkipsStartVMOnTarget pins the live convergence rule: for
// a LIVE migration the target QEMU self-resumes the guest at switchover, so the
// worker must NOT call StartVMOnTarget (a start there is a no-op-or-error).
// DeleteVMOnSource still runs (behind the committed cutover). The target's
// nbd_endpoint from StartIncomingMigration must be relayed into the outgoing
// request so the source dials the right NBD disk-export listener.
func TestDriveHandshake_LiveSkipsStartVMOnTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID) // DesiredPhase=running.
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	// Explicit (bound) LIVE migration: skips placement, goes straight to handshake.
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	nbd := "h:49153"
	agent := &fakeMigrationAgent{
		terminal:            agentclient.TaskTerminal{Status: "success"},
		incomingNbdEndpoint: &nbd,
	}
	placer := &fakePlacer{} // must NOT be called: the migration is already bound.

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(live) = %v, want nil", err)
	}

	if agent.startTargetCalls != 0 {
		t.Errorf("live migration called StartVMOnTarget %d times, want 0 (guest self-resumes)", agent.startTargetCalls)
	}
	if agent.deleteSourceCalls != 1 {
		t.Errorf("live migration called DeleteVMOnSource %d times, want 1", agent.deleteSourceCalls)
	}
	if agent.outgoingReq.NbdEndpoint == nil || *agent.outgoingReq.NbdEndpoint != "h:49153" {
		t.Errorf("nbd_endpoint relayed = %v, want h:49153", agent.outgoingReq.NbdEndpoint)
	}
}

// TestDriveHandshake_OfflineStillStartsVMOnTarget is the revert-to-confirm: an
// OFFLINE migration whose VM desires running STILL calls StartVMOnTarget (the
// target adopts a stopped VM and the CP starts it). The live gate must not change
// the 2a offline behaviour.
func TestDriveHandshake_OfflineStillStartsVMOnTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID) // DesiredPhase=running.
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	// Explicit (bound) OFFLINE migration.
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", false)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{} // must NOT be called: the migration is already bound.

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(offline) = %v, want nil", err)
	}

	if agent.startTargetCalls != 1 {
		t.Errorf("offline migration called StartVMOnTarget %d times, want 1 (CP starts the adopted stopped VM)", agent.startTargetCalls)
	}
	if agent.deleteSourceCalls != 1 {
		t.Errorf("offline migration called DeleteVMOnSource %d times, want 1", agent.deleteSourceCalls)
	}
}

// derefStr returns the pointee of a *string, or "" when nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestRunMigration_PendingSingleNode pins the D4 retry-forever path: the placer
// reports no eligible node, so the migration stays pending with
// scheduling_reason=no_eligible_target, the worker returns a RETRYABLE error
// (requeue), and the agent is NEVER contacted.
func TestRunMigration_PendingSingleNode(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "only-node", "https://only:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	// The empty-pool default-to-source path needs a resolvable source pool; the
	// pending verdict here comes from the placer (no eligible node), not the pool.
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{}
	placer := &fakePlacer{err: scheduler.ErrNoEligibleNodes}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	err := h(ctx, jobArgs(t, taskID, m.ID))
	if err == nil {
		t.Fatalf("MigrateHandler(pending) = nil, want a retryable error so the dispatcher requeues")
	}

	got, err2 := s.MigrationByID(ctx, m.ID)
	if err2 != nil {
		t.Fatalf("MigrationByID: %v", err2)
	}
	if got.Phase != store.MigrationPhasePending {
		t.Errorf("migration phase = %q, want pending", got.Phase)
	}
	if got.SchedulingReason == nil || *got.SchedulingReason != migrations.ReasonNoEligibleTarget {
		t.Errorf("scheduling_reason = %v, want %q", got.SchedulingReason, migrations.ReasonNoEligibleTarget)
	}
	if got.TargetNodeID != nil {
		t.Errorf("TargetNodeID = %v, want nil (no target bound on pending)", got.TargetNodeID)
	}
	if got.LastScheduleAttemptAt == nil {
		t.Errorf("LastScheduleAttemptAt = nil, want stamped")
	}
	if agent.incomingCalls != 0 || agent.outgoingCalls != 0 || agent.pollCalls != 0 {
		t.Errorf("agent contacted on the pending path: incoming=%d outgoing=%d poll=%d, want 0/0/0",
			agent.incomingCalls, agent.outgoingCalls, agent.pollCalls)
	}
}

// TestPlaceAndBindRefusesNetworkUnreadyTarget verifies that a node-less
// migration must honor the scheduler's network-readiness filter
// exactly as vm create does. The only resource-eligible target (nodeB) has the
// VM's bridge network in a NON-ready state, so placeAndBind must pass the VM's
// NetworkIDs to the placer and the candidate is excluded - the migration stays
// PENDING, binds NO target, and the agent is NEVER contacted. It drives the REAL
// placer (NewSchedulerPlacer over the store) so the network filter actually runs;
// a fakePlacer would bypass the filter entirely.
func TestPlaceAndBindRefusesNetworkUnreadyTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)

	// Source pool on A (empty-pool path resolves the source pool name); nodeB has
	// the same pool name so it is a resource-eligible candidate.
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default-on-b")

	// The VM has a NIC on bridge network N. nodeB's per-(node, network) status for
	// N is NOT ready (pending) -> the network filter must exclude nodeB, the only
	// candidate (nodeA is the excluded source).
	net := seedBridgeNetwork(t, s, "br-net", "otvb0")
	seedVMNic(t, cli, vm.ID, net.ID, "52:54:00:00:00:01")
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: net.ID, NodeID: nodeB.ID, ReconciliationStatus: "pending",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus(nodeB pending): %v", err)
	}

	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{}
	// REAL placer: the scheduler runs filterByNetworkReadiness against the seeded
	// per-(node, network) status. Zero-value Resources keeps the resource fit a
	// pass-through so the candidate reaches the network filter.
	placer := migrations.NewSchedulerPlacer(s.PlacementQuerier(), scheduler.PlacementConfig{})

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	err := h(ctx, jobArgs(t, taskID, m.ID))
	if err == nil {
		t.Fatalf("MigrateHandler(network-unready) = nil, want a retryable error so the dispatcher requeues")
	}

	got, err2 := s.MigrationByID(ctx, m.ID)
	if err2 != nil {
		t.Fatalf("MigrationByID: %v", err2)
	}
	if got.Phase != store.MigrationPhasePending {
		t.Errorf("migration phase = %q, want pending (network filter excluded the only candidate)", got.Phase)
	}
	if got.TargetNodeID != nil {
		t.Errorf("TargetNodeID = %v, want nil (must NOT cut over to the network-unready node)", got.TargetNodeID)
	}
	if got.SchedulingReason == nil || *got.SchedulingReason != migrations.ReasonNoEligibleTarget {
		t.Errorf("scheduling_reason = %v, want %q", got.SchedulingReason, migrations.ReasonNoEligibleTarget)
	}
	// The agent was NEVER contacted: nothing durable moved, VM stays on source.
	if agent.incomingCalls != 0 || agent.outgoingCalls != 0 || agent.pollCalls != 0 {
		t.Errorf("agent contacted on the network-unready pending path: incoming=%d outgoing=%d poll=%d, want 0/0/0",
			agent.incomingCalls, agent.outgoingCalls, agent.pollCalls)
	}
	// PinnedNodeID unchanged: the VM is still on its source.
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v (pending, no cutover)", gotVM.PinnedNodeID, nodeA.ID)
	}
}

// TestRunMigration_FailurePreCutover pins the fail-safe-to-source rule: the
// source outgoing task fails (target_unreachable) before cutover, so the
// migration ends failed, ErrorMessage is set, and PinnedNodeID is UNCHANGED
// (still source) - cutover never ran.
func TestRunMigration_FailurePreCutover(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{
		Status: "failed",
		Error:  &agentclient.AgentError{Status: 502, Code: migrations.ErrCodeTargetUnreachable, Message: "target unreachable"},
	}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(failure) = %v, want nil (terminal, not requeued)", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseFailed {
		t.Errorf("migration phase = %q, want failed", got.Phase)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage == "" {
		t.Errorf("ErrorMessage = %v, want set", got.ErrorMessage)
	}
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v (fail-safe-to-source)", gotVM.PinnedNodeID, nodeA.ID)
	}
	// Fail-closed: post-cutover convergence is reached ONLY after a committed
	// cutover. A pre-cutover failure must NEVER delete the source's copy.
	if agent.deleteSourceCalls != 0 {
		t.Errorf("DeleteVMOnSource calls = %d, want 0 (no committed cutover - source must NOT be deleted)", agent.deleteSourceCalls)
	}
	if agent.startTargetCalls != 0 {
		t.Errorf("StartVMOnTarget calls = %d, want 0 (no committed cutover)", agent.startTargetCalls)
	}
}

// TestDriveHandshake_LiveSourceFailureCancelsTarget pins the rule: when a LIVE
// migration's source outgoing task ends terminal-failure pre-cutover, the worker
// tells the bound TARGET to reap its incoming setup (best-effort
// CancelMigration) so the target does not leak until its 30-minute incoming
// timeout. Driven through the real worker entry (MigrateHandler).
func TestDriveHandshake_LiveSourceFailureCancelsTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	// Explicit (bound) LIVE migration so the bound target is known.
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{
		Status: "failed",
		Error:  &agentclient.AgentError{Status: 500, Code: migrations.ErrCodeConvergenceFailed, Message: "boom"},
	}}
	placer := &fakePlacer{} // must NOT be called: the migration is already bound.

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(live-source-failure) = %v, want nil (terminal, not requeued)", err)
	}

	if len(agent.cancelCalls) != 1 {
		t.Fatalf("cancelCalls = %v, want one CancelMigration(target)", agent.cancelCalls)
	}
	call := agent.cancelCalls[0]
	if call.endpoint != nodeB.AdvertisedEndpoint {
		t.Errorf("cancel endpoint = %q, want target %q", call.endpoint, nodeB.AdvertisedEndpoint)
	}
	if call.vmName != vm.Name {
		t.Errorf("cancel vmName = %q, want %q", call.vmName, vm.Name)
	}
	if call.migID != m.ID.String() {
		t.Errorf("cancel migID = %q, want %q", call.migID, m.ID.String())
	}
}

// TestDriveHandshake_LiveSuccessDoesNotCancelTarget is the revert-to-confirm:
// a successful LIVE migration must NOT cancel the target - the target IS
// the now-running guest, and the cutover committed.
func TestDriveHandshake_LiveSuccessDoesNotCancelTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(live-success) = %v, want nil", err)
	}

	if len(agent.cancelCalls) != 0 {
		t.Errorf("cancelCalls = %v, want none on success", agent.cancelCalls)
	}
}

// commitCutoverFailStore wraps a real MigrationWorkerStore and forces
// CommitMigrationCutover to return a staged error, so a test can drive the
// dangerous interleaving where the source poll returns success but the atomic
// cutover commit then fails (CAS loss / store fault). Every other method
// delegates to the embedded real store.
type commitCutoverFailStore struct {
	migrations.MigrationWorkerStore
	commitCutoverErr error
}

func (st commitCutoverFailStore) CommitMigrationCutover(ctx context.Context, migID uuid.UUID) error {
	if st.commitCutoverErr != nil {
		return st.commitCutoverErr
	}
	return st.MigrationWorkerStore.CommitMigrationCutover(ctx, migID)
}

// statsErrStore wraps a real MigrationWorkerStore and forces UpdateMigrationStats
// to fail, exercising the fail-open path (a stats store error must not fail a
// committed cutover).
type statsErrStore struct {
	migrations.MigrationWorkerStore
}

func (statsErrStore) UpdateMigrationStats(ctx context.Context, migID uuid.UUID, stats store.MigrationStats) error {
	return errors.New("stats store boom")
}

// overlayPeersStub wraps a real MigrationWorkerStore and returns a settable
// peer-node slice from OverlayPeerNodesForVM, so a worker test can assert the
// post-cutover fast-push nudges those peers (plus the target) without seeding a
// full overlay NIC topology. Every other method delegates to the embedded store.
type overlayPeersStub struct {
	migrations.MigrationWorkerStore
	peers []store.Node
}

func (st overlayPeersStub) OverlayPeerNodesForVM(_ context.Context, _ uuid.UUID) ([]store.Node, error) {
	return st.peers, nil
}

// TestConvergePostCutoverNudgesOverlayPeers pins the fast-push:
// on the committed-cutover success arm, the worker nudges the computed overlay
// peer set AND the target so they re-pull their FDB immediately. It drives the
// full happy path (convergePostCutover is unexported), injecting the peer set
// through an overlayPeersStub.
func TestConvergePostCutoverNudgesOverlayPeers(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443") // target
	peer := seedReadyNode(t, s, "node-peer", "https://node-peer:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, false)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	st := overlayPeersStub{MigrationWorkerStore: s, peers: []store.Node{peer}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	h := migrations.MigrateHandler(st, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(happy) = %v, want nil", err)
	}

	agent.mu.Lock()
	nudged := map[string]bool{}
	for _, ep := range agent.nudged {
		nudged[ep] = true
	}
	agent.mu.Unlock()
	if !nudged[nodeB.AdvertisedEndpoint] {
		t.Errorf("target endpoint %q not nudged; nudged=%v", nodeB.AdvertisedEndpoint, nudged)
	}
	if !nudged[peer.AdvertisedEndpoint] {
		t.Errorf("overlay peer endpoint %q not nudged; nudged=%v", peer.AdvertisedEndpoint, nudged)
	}
}

// TestRunMigration_CommitFailsAfterSuccessKeepsSource pins the destructive-seam
// guard: the source outgoing task polls SUCCESS, but CommitMigrationCutover then
// ERRORS (CAS loss / store fault). The cutover never committed, so the worker
// must return the commit error (retryable) and must NOT run post-cutover
// convergence - the source's copy is NOT deleted and the guest is NOT started on
// the target. DeleteVMOnSource is destructive (deletes the source disk); it may
// fire ONLY after a committed cutover.
func TestRunMigration_CommitFailsAfterSuccessKeepsSource(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	// The poll terminal is success, so the worker reaches commitCutover; the store
	// wrapper forces that commit to fail.
	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}
	st := commitCutoverFailStore{MigrationWorkerStore: s, commitCutoverErr: errors.New("commit cutover boom")}

	h := migrations.MigrateHandler(st, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err == nil {
		t.Fatalf("MigrateHandler(commit-fails) = nil, want the commit error to propagate (retryable)")
	}

	// KEY assertions: no committed cutover means no destructive post-cutover steps.
	if agent.deleteSourceCalls != 0 {
		t.Errorf("DeleteVMOnSource calls = %d, want 0 (commit failed - source must NOT be deleted)", agent.deleteSourceCalls)
	}
	if agent.startTargetCalls != 0 {
		t.Errorf("StartVMOnTarget calls = %d, want 0 (commit failed - no cutover)", agent.startTargetCalls)
	}
	// The VM stays pinned to source: the cutover never re-pinned it.
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v (commit failed)", gotVM.PinnedNodeID, nodeA.ID)
	}
}

// TestRunMigration_PersistsStatsFromTaskResult drives the full live happy path
// and pins that, on a committed cutover, the worker parses the final live-
// migration stats out of the terminal task Result and persists them on the
// migration row. The agent terminal carries the locked stats wire shape.
func TestRunMigration_PersistsStatsFromTaskResult(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{
		Status: "success",
		Result: map[string]any{
			"migration_id": "ignored",
			"stats": map[string]any{
				"ram":           map[string]any{"transferred": 10737418240.0, "total": 10737418240.0, "dirty_pages_rate": 7.0},
				"disk":          map[string]any{"transferred": 5368709120.0, "total": 5368709120.0},
				"total_time_ms": 45125.0, "downtime_ms": 150.0, "setup_time_ms": 1200.0,
			},
		},
	}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(happy) = %v, want nil", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Stats == nil {
		t.Fatalf("Stats = nil, want populated")
	}
	want := store.MigrationStats{
		RAMTransferred: 10737418240, RAMTotal: 10737418240, RAMDirtyPagesRate: 7,
		DiskTransferred: 5368709120, DiskTotal: 5368709120,
		TotalTimeMs: 45125, DowntimeMs: 150, SetupTimeMs: 1200,
	}
	if diff := cmp.Diff(want, *got.Stats); diff != "" {
		t.Errorf("Stats mismatch (-want +got):\n%s", diff)
	}
}

// TestRunMigration_FailOpenOnMissingStats pins the fail-open invariant for the
// common case (offline migration / old agent): the terminal Result carries no
// "stats" object. The migration must still complete and Stats must stay nil.
func TestRunMigration_FailOpenOnMissingStats(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{
		Status: "success",
		Result: map[string]any{"migration_id": "ignored"},
	}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(no-stats) = %v, want nil", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Stats != nil {
		t.Errorf("Stats = %+v, want nil (no stats in result)", got.Stats)
	}
	if got.Phase != store.MigrationPhaseCompleted {
		t.Errorf("Phase = %q, want completed", got.Phase)
	}
}

// TestRunMigration_FailOpenOnStatsStoreError pins the fail-open invariant for a
// store fault: the committed cutover already landed, then UpdateMigrationStats
// errors. The handler must still return nil and the migration must stay
// completed - stats are observability only and never block a committed cutover.
func TestRunMigration_FailOpenOnStatsStoreError(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{
		Status: "success",
		Result: map[string]any{
			"stats": map[string]any{
				"ram":           map[string]any{"transferred": 10737418240.0, "total": 10737418240.0, "dirty_pages_rate": 7.0},
				"disk":          map[string]any{"transferred": 5368709120.0, "total": 5368709120.0},
				"total_time_ms": 45125.0, "downtime_ms": 150.0, "setup_time_ms": 1200.0,
			},
		},
	}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}
	st := statsErrStore{MigrationWorkerStore: s}

	h := migrations.MigrateHandler(st, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(stats-store-error) = %v, want nil (stats store error must not fail cutover)", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCompleted {
		t.Errorf("Phase = %q, want completed (stats store error must not fail cutover)", got.Phase)
	}
}

// TestRunMigration_FinalizesDanglingTaskCompleted pins the A2.4 fix: a crash
// between CommitMigrationCutover (migration -> completed) and the task finalize
// leaves the migration terminal but the backing task still non-terminal. On
// redelivery the worker hits the migration-already-terminal short-circuit; it
// MUST finalize the dangling task to match the migration outcome (completed ->
// success) instead of returning nil with the task stuck running forever. The
// agent is never contacted in this reconcile-by-query.
func TestRunMigration_FinalizesDanglingTaskCompleted(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	// Simulate the crash window: bind + commit the cutover so the migration is
	// terminal-completed, but DO NOT finalize the task (it stays pending).
	if err := s.BindMigrationTarget(ctx, m.ID, nodeB.ID, "default"); err != nil {
		t.Fatalf("BindMigrationTarget: %v", err)
	}
	if err := s.CommitMigrationCutover(ctx, m.ID); err != nil {
		t.Fatalf("CommitMigrationCutover: %v", err)
	}
	before, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID(before): %v", err)
	}
	if before.Status == store.TaskStatusSuccess {
		t.Fatalf("precondition violated: task already success before redelivery")
	}

	agent := &fakeMigrationAgent{} // must NOT be contacted.
	placer := &fakePlacer{}        // must NOT be called.

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(redelivery) = %v, want nil", err)
	}

	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID(after): %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task status = %q, want success (dangling task finalized to match completed migration)", task.Status)
	}
	if task.FinishedAt == nil {
		t.Errorf("task FinishedAt = nil, want stamped on finalize")
	}
	if agent.incomingCalls != 0 || agent.outgoingCalls != 0 || agent.pollCalls != 0 {
		t.Errorf("agent contacted on reconcile-by-query: incoming=%d outgoing=%d poll=%d, want 0/0/0",
			agent.incomingCalls, agent.outgoingCalls, agent.pollCalls)
	}
	if placer.calls != 0 {
		t.Errorf("placer called %d times, want 0", placer.calls)
	}
}

// TestRunMigration_FinalizesDanglingTaskFailed pins the A2.4 fix for the failed
// branch: a failed migration is NOT committed-terminal, so UpdateTaskRunning does
// NOT short-circuit and the worker reaches the migration-already-terminal branch
// with the task transitioned to running. It MUST finalize the task to failed to
// match the migration, not leave it stuck running.
func TestRunMigration_FinalizesDanglingTaskFailed(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	// Drive the migration to terminal-failed via the store, leaving the task
	// un-finalized (the crash window between the migration fail-write and the task
	// finalize).
	failed := store.MigrationPhaseFailed
	msg := "convergence stalled"
	if err := s.UpdateMigrationProgress(ctx, m.ID, store.MigrationProgressUpdate{Phase: &failed, ErrorMessage: &msg}); err != nil {
		t.Fatalf("UpdateMigrationProgress(failed): %v", err)
	}

	agent := &fakeMigrationAgent{} // must NOT be contacted.
	placer := &fakePlacer{}        // must NOT be called.

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(redelivery) = %v, want nil", err)
	}

	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID(after): %v", err)
	}
	if task.Status != store.TaskStatusFailed {
		t.Errorf("task status = %q, want failed (dangling task finalized to match failed migration)", task.Status)
	}
	if task.FinishedAt == nil {
		t.Errorf("task FinishedAt = nil, want stamped on finalize")
	}
	if agent.incomingCalls != 0 || agent.outgoingCalls != 0 || agent.pollCalls != 0 {
		t.Errorf("agent contacted on reconcile-by-query: incoming=%d outgoing=%d poll=%d, want 0/0/0",
			agent.incomingCalls, agent.outgoingCalls, agent.pollCalls)
	}
}

// TestFinalizeForTerminal_CompletedDoesNotReapTarget is the destructive-seam
// guard: a COMPLETED live migration's target IS the running guest.
// The reconcile arm must NOT cancel it - reaping would destroy the live VM.
func TestFinalizeForTerminal_CompletedDoesNotReapTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	// Bind + commit the cutover so the migration is terminal-completed with a bound
	// target, leaving the task un-finalized (the crash-reconcile window).
	if err := s.BindMigrationTarget(ctx, m.ID, nodeB.ID, "default"); err != nil {
		t.Fatalf("BindMigrationTarget: %v", err)
	}
	if err := s.CommitMigrationCutover(ctx, m.ID); err != nil {
		t.Fatalf("CommitMigrationCutover: %v", err)
	}

	agent := &fakeMigrationAgent{}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(completed-reconcile) = %v, want nil", err)
	}

	if len(agent.cancelCalls) != 0 {
		t.Errorf("cancelCalls = %v, want none for completed (target IS the live VM)", agent.cancelCalls)
	}
}

// TestRunMigration_ResumesWithoutDoublePost pins the agent_task_id resumption
// (R2-M2 analogue): a redelivered job whose task already carries an agent task id
// must NOT re-run the two-phase handshake (no double incoming/outgoing POST); it
// resumes by polling the persisted source task. The migration is already bound,
// so placement is skipped too.
func TestRunMigration_ResumesWithoutDoublePost(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	// Bind the target and persist an agent task id: this is the post-restart state
	// a redelivery resumes from (the source push already started on a prior run).
	if err := s.BindMigrationTarget(ctx, m.ID, nodeB.ID, "default"); err != nil {
		t.Fatalf("BindMigrationTarget: %v", err)
	}
	resumeID := uuid.New()
	if err := s.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &resumeID}); err != nil {
		t.Fatalf("UpdateTaskAgentTaskID: %v", err)
	}

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}, agentTaskID: resumeID}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{}} // must not be called.

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(resume) = %v, want nil", err)
	}

	if agent.incomingCalls != 0 || agent.outgoingCalls != 0 {
		t.Errorf("redelivery re-POSTed the handshake: incoming=%d outgoing=%d, want 0/0 (resume only)",
			agent.incomingCalls, agent.outgoingCalls)
	}
	if agent.pollCalls != 1 {
		t.Errorf("poll calls = %d, want 1 (resume polls the persisted source task)", agent.pollCalls)
	}
	if placer.calls != 0 {
		t.Errorf("placer called %d times on an already-bound migration, want 0", placer.calls)
	}
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want target %v (resume drove cutover)", gotVM.PinnedNodeID, nodeB.ID)
	}
}

// placementLockSpy wraps a real MigrationWorkerStore and records the call order
// of AcquirePlacementLock vs BindMigrationTarget, so a test can prove the worker
// holds store.LockKeyPlacement across the placement -> bind decision window (the
// TOCTOU guard mirroring the vm.create path). It delegates every method to the
// embedded real store; only the two ordering-relevant calls are observed.
type placementLockSpy struct {
	migrations.MigrationWorkerStore
	mu       sync.Mutex
	events   []string
	lockKeys []int64
}

func (sp *placementLockSpy) AcquirePlacementLock(ctx context.Context, lockKey int64) (func(), error) {
	sp.mu.Lock()
	sp.events = append(sp.events, "lock")
	sp.lockKeys = append(sp.lockKeys, lockKey)
	sp.mu.Unlock()
	release, err := sp.MigrationWorkerStore.AcquirePlacementLock(ctx, lockKey)
	if err != nil {
		return release, err
	}
	return func() {
		sp.mu.Lock()
		sp.events = append(sp.events, "unlock")
		sp.mu.Unlock()
		release()
	}, nil
}

func (sp *placementLockSpy) BindMigrationTarget(ctx context.Context, migID, targetNodeID uuid.UUID, poolName string) error {
	sp.mu.Lock()
	sp.events = append(sp.events, "bind")
	sp.mu.Unlock()
	return sp.MigrationWorkerStore.BindMigrationTarget(ctx, migID, targetNodeID, poolName)
}

// TestRunMigration_AcquiresPlacementLockBeforeBind pins the TOCTOU guard: on the
// node-less placement path the worker acquires store.LockKeyPlacement BEFORE it
// binds the chosen target, so two concurrent node-less migrations serialize their
// target choice across replicas (the lock is a no-op single-node stub today, so
// this asserts the structural acquire/order, not contention behavior).
func TestRunMigration_AcquiresPlacementLockBeforeBind(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID, true)

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{decision: scheduler.PlacementDecision{
		Node:         store.NodeEffectiveAvailability{ID: nodeB.ID, Name: nodeB.Name, Status: store.NodeStatusReady},
		PoolInstance: store.PoolEffectiveCapacity{Name: "default"},
	}}

	spy := &placementLockSpy{MigrationWorkerStore: s}
	h := migrations.MigrateHandler(spy, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(lock-order) = %v, want nil", err)
	}

	if got := spy.events; len(got) < 3 || got[0] != "lock" || got[1] != "bind" || got[2] != "unlock" {
		t.Errorf("call order = %v, want [lock bind unlock] (lock held across placement -> bind, released after)", got)
	}
	if len(spy.lockKeys) == 0 || spy.lockKeys[0] != store.LockKeyPlacement {
		t.Errorf("lock key = %v, want first acquire on store.LockKeyPlacement (%d)", spy.lockKeys, store.LockKeyPlacement)
	}
}

// TestStartOrResumeSetsExpectedSizeFromSource pins Fix A: the worker fetches the
// SOURCE VM's real boot-disk virtual size (via agent.VM) and threads it onto the
// incoming request's ExpectedSizeBytes, so the target's max(SizeBytes, ExpectedSize)
// guard sizes the destination disk correctly even when the VM was created without
// --disk-gib (SizeGib=0). Driven through the REAL worker entry (MigrateHandler).
func TestStartOrResumeSetsExpectedSizeFromSource(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "src-pool", "/var/lib/otherix/pools/src-pool-on-a")
	// Boot disk row carries SizeGib=0 (image-sized VM, no --disk-gib): the manifest
	// would otherwise send a 0-byte destination disk. ExpectedSizeBytes is the fix.
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 0)
	seedPool(t, s, nodeB.ID, "src-pool", "/var/lib/otherix/pools/src-pool-on-b")

	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "", true)

	const srcSize int64 = 3758096384 // 3.5 GiB, the source's real virtual size.
	agent := &fakeMigrationAgent{
		terminal: agentclient.TaskTerminal{Status: "success"},
		sourceVM: agentclient.AgentVM{BootDiskVirtualSizeBytes: srcSize},
	}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(expected-size) = %v, want nil", err)
	}

	got := agent.incomingReq.ExpectedSizeBytes
	if got == nil {
		t.Fatalf("incoming ExpectedSizeBytes = nil, want %d (sourced from agent.VM boot disk size)", srcSize)
	}
	if *got != srcSize {
		t.Errorf("incoming ExpectedSizeBytes = %d, want %d (the source's real virtual size)", *got, srcSize)
	}
}

// cancelWhilePollingStore wraps a real MigrationWorkerStore and, the FIRST time
// PollTask-equivalent terminal handling is about to run, drives the migration to
// terminal-cancelled via the embedded store. It models the operator CP-side
// cancel landing while the worker is blocked in PollTask: by the time the source
// task returns terminal, the migration is already cancelled (the authoritative
// state). It hooks NodeByID (called immediately before the terminal switch in
// driveHandshake) is too early; instead it hooks the cutover-irrelevant
// UpdateMigrationProgress to observe the advancePhase->active call and arm a
// one-shot cancel right after, so the subsequent PollTask terminal lands on an
// already-cancelled migration. Every other method delegates to the real store.
type cancelWhilePollingStore struct {
	migrations.MigrationWorkerStore
	migID    uuid.UUID
	armed    bool
	cancelFn func()
}

func (st *cancelWhilePollingStore) UpdateMigrationProgress(ctx context.Context, migID uuid.UUID, upd store.MigrationProgressUpdate) error {
	// Let the real advance to active happen, then cancel the migration once so the
	// next terminal handling sees an already-cancelled (terminal) migration.
	err := st.MigrationWorkerStore.UpdateMigrationProgress(ctx, migID, upd)
	if !st.armed && upd.Phase != nil && *upd.Phase == store.MigrationPhaseActive && st.cancelFn != nil {
		st.armed = true
		st.cancelFn()
	}
	return err
}

// TestDriveHandshake_CancelledDuringPollFinalizesCancelled pins Change 2: when
// the operator cancels the migration (CP-side, store -> cancelled) while the
// worker is driving the handshake, the source outgoing task subsequently returns
// terminal. The worker must NOT try failMigration (that would hit
// ErrMigrationTerminal and burn a retry with a misleading "migration failed"
// log); instead it reloads the migration, sees it is terminal-cancelled, and
// finalizes the backing task to CANCELLED (not failed) while reaping the bound
// target's incoming. The handler returns nil (no requeue).
func TestDriveHandshake_CancelledDuringPollFinalizesCancelled(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	// Explicit (bound) LIVE migration so the bound target is known and skips placement.
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	// The source task reports terminal-failed (the agent's source-side abort surfaces
	// the in-flight migration as a failed/cancelled outgoing task). Because the
	// migration is already cancelled by the time we reach the terminal switch, the
	// worker must map to a CANCELLED task, not a FAILED one.
	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{
		Status: "failed",
		Error:  &agentclient.AgentError{Status: 500, Code: migrations.ErrCodeConvergenceFailed, Message: "aborted by source"},
	}}
	placer := &fakePlacer{} // must NOT be called: already bound.

	st := &cancelWhilePollingStore{MigrationWorkerStore: s, migID: m.ID}
	st.cancelFn = func() {
		if _, err := s.CancelMigration(ctx, m.ID, "operator cancelled mid-flight"); err != nil {
			t.Fatalf("mid-flight CancelMigration: %v", err)
		}
	}

	h := migrations.MigrateHandler(st, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(cancelled-during-poll) = %v, want nil (terminal-cancelled, not requeued)", err)
	}

	// The migration stays cancelled (never overwritten to failed).
	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("migration phase = %q, want cancelled (must NOT be overwritten to failed)", got.Phase)
	}

	// The backing task is finalized CANCELLED, matching the migration, not failed.
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusCancelled {
		t.Errorf("task status = %q, want cancelled (cancelled migration -> cancelled task, not failed)", task.Status)
	}

	// The bound target's incoming was reaped (best-effort).
	if len(agent.cancelCalls) != 1 {
		t.Fatalf("cancelCalls = %v, want one target reap for the cancelled live migration", agent.cancelCalls)
	}
	call := agent.cancelCalls[0]
	if call.endpoint != nodeB.AdvertisedEndpoint || call.vmName != vm.Name || call.migID != m.ID.String() {
		t.Errorf("cancel call = %+v, want {endpoint:%q vmName:%q migID:%q}", call, nodeB.AdvertisedEndpoint, vm.Name, m.ID.String())
	}

	// The VM stays on its source: a cancelled migration never re-pins.
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v (cancel never cuts over)", gotVM.PinnedNodeID, nodeA.ID)
	}
}

func jobArgs(t *testing.T, taskID, migID uuid.UUID) []byte {
	t.Helper()
	b, err := json.Marshal(migrations.MigrationRunArgs{TaskID: taskID, MigrationID: migID})
	if err != nil {
		t.Fatalf("marshal run args: %v", err)
	}
	return b
}

// armCancelledReconcile prepares the redelivery-of-a-cancelled-migration
// fixture: it persists agentTaskID on the backing task (the source handshake
// started, so finalizeForTerminalMigration must poll the source) and then drives
// the migration to terminal-cancelled. The next handler delivery short-circuits
// at the runMigration terminal check and routes through
// finalizeForTerminalMigration, which arbitrates on the source-task poll.
func armCancelledReconcile(t *testing.T, s *etcdstore.Store, taskID, migID, agentTaskID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	at := agentTaskID
	if err := s.UpdateTaskAgentTaskID(ctx, store.UpdateTaskAgentTaskIDParams{ID: taskID, AgentTaskID: &at}); err != nil {
		t.Fatalf("persist agent_task_id: %v", err)
	}
	if _, err := s.CancelMigration(ctx, migID, "operator cancelled"); err != nil {
		t.Fatalf("cancel migration: %v", err)
	}
}

// TestFinalizeTerminalCancelled_SourceSuccessCutsOver pins the first stranded
// path: a CANCELLED migration is redelivered, but a poll of the AUTHORITATIVE
// source task reports SUCCESS - the source already handed off (destroyed) and the
// only live copy is on the target. finalizeForTerminalMigration MUST commit the
// cutover (re-pin VM to target), converge post-cutover (DeleteVMOnSource), and
// finalize the task SUCCESS. The target is NEVER reaped (it is the running VM).
//
// Revert-to-confirm: without the source-status arbitration the cancelled arm
// finalizes the task cancelled and reaps the target, leaving the VM pinned to the
// destroyed source - split-brain / VM-loss.
func TestFinalizeTerminalCancelled_SourceSuccessCutsOver(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	armCancelledReconcile(t, s, taskID, m.ID, uuid.New())

	// The source task reports SUCCESS on the reconcile poll: the source handed off.
	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "success"}}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(cancelled+source-success) = %v, want nil (cutover to target)", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCompleted {
		t.Errorf("migration phase = %q, want completed (source handed off; cutover overrides the cancel)", got.Phase)
	}
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeB.ID {
		t.Errorf("PinnedNodeID = %v, want target %v (no strand on the destroyed source)", gotVM.PinnedNodeID, nodeB.ID)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Errorf("task status = %q, want success", task.Status)
	}
	// The target is the running VM: it must NEVER be reaped.
	if len(agent.cancelCalls) != 0 {
		t.Errorf("cancelCalls = %v, want none (target is the only live copy)", agent.cancelCalls)
	}
	// Post-cutover convergence ran: DeleteVMOnSource attempted (no-op since the
	// source is already gone, but it must be on the success arm).
	if agent.deleteSourceCalls != 1 {
		t.Errorf("DeleteVMOnSource calls = %d, want 1 (committed cutover converges)", agent.deleteSourceCalls)
	}
}

// TestFinalizeTerminalCancelled_SourceFailedReapsTarget pins the genuine
// pre-switchover cancel: a CANCELLED migration is redelivered and the source task
// reports FAILED (the source aborted; the guest is still alive on source).
// finalizeForTerminalMigration must reap the target's incoming, finalize the task
// CANCELLED, and leave the VM on its source.
func TestFinalizeTerminalCancelled_SourceFailedReapsTarget(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	armCancelledReconcile(t, s, taskID, m.ID, uuid.New())

	agent := &fakeMigrationAgent{terminal: agentclient.TaskTerminal{Status: "failed"}}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(cancelled+source-failed) = %v, want nil", err)
	}

	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("migration phase = %q, want cancelled (source aborted; VM stays on source)", got.Phase)
	}
	gotVM, err := s.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID: %v", err)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v (cancel never cuts over)", gotVM.PinnedNodeID, nodeA.ID)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusCancelled {
		t.Errorf("task status = %q, want cancelled", task.Status)
	}
	// The bound target's incoming is reaped (best-effort) since the source aborted.
	if len(agent.cancelCalls) != 1 {
		t.Fatalf("cancelCalls = %v, want one target reap (source aborted)", agent.cancelCalls)
	}
	if agent.cancelCalls[0].endpoint != nodeB.AdvertisedEndpoint {
		t.Errorf("cancel endpoint = %q, want target %q", agent.cancelCalls[0].endpoint, nodeB.AdvertisedEndpoint)
	}
	if agent.deleteSourceCalls != 0 {
		t.Errorf("DeleteVMOnSource calls = %d, want 0 (no cutover; source keeps the VM)", agent.deleteSourceCalls)
	}
}

// TestFinalizeTerminalCancelled_SourcePollErrorRetries pins the fail-toward-
// inaction rule: a CANCELLED migration is redelivered and the source-task poll
// ERRORS (source outcome UNKNOWN). finalizeForTerminalMigration must return a
// RETRYABLE error and touch nothing - the migration stays cancelled, the task is
// NOT finalized, and the target is NEVER reaped (it could be the only live copy).
func TestFinalizeTerminalCancelled_SourcePollErrorRetries(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	armCancelledReconcile(t, s, taskID, m.ID, uuid.New())

	agent := &fakeMigrationAgent{pollErr: errors.New("source unreachable")}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	err := h(ctx, jobArgs(t, taskID, m.ID))
	if err == nil {
		t.Fatalf("MigrateHandler(cancelled+poll-error) = nil, want a retryable error (source outcome unknown -> never destroy)")
	}

	got, err2 := s.MigrationByID(ctx, m.ID)
	if err2 != nil {
		t.Fatalf("MigrationByID: %v", err2)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("migration phase = %q, want UNCHANGED cancelled (no decision on uncertainty)", got.Phase)
	}
	gotVM, err3 := s.VMByID(ctx, vm.ID)
	if err3 != nil {
		t.Fatalf("VMByID: %v", err3)
	}
	if gotVM.PinnedNodeID == nil || *gotVM.PinnedNodeID != nodeA.ID {
		t.Errorf("PinnedNodeID = %v, want UNCHANGED source %v", gotVM.PinnedNodeID, nodeA.ID)
	}
	// The task is NOT finalized: a poll error is retryable, not terminal.
	task, err4 := s.TaskByID(ctx, taskID)
	if err4 != nil {
		t.Fatalf("TaskByID: %v", err4)
	}
	if task.Status == store.TaskStatusCancelled || task.Status == store.TaskStatusSuccess || task.Status == store.TaskStatusFailed {
		t.Errorf("task status = %q, want NOT finalized (poll error is retryable)", task.Status)
	}
	// The target is NEVER reaped on uncertainty (it could be the only live copy).
	if len(agent.cancelCalls) != 0 {
		t.Errorf("cancelCalls = %v, want none (uncertain source -> do not destroy the target)", agent.cancelCalls)
	}
}

// TestFinalizeTerminalCancelled_PendingNoAgentTaskNoReap pins the cancelled-while-
// pending path: a migration cancelled before the source handshake started (no
// AgentTaskID on the task) is redelivered. Nothing moved; the source was never
// contacted. finalizeForTerminalMigration must finalize the task CANCELLED with NO
// source poll and NO target reap.
func TestFinalizeTerminalCancelled_PendingNoAgentTaskNoReap(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	srcPool := seedPool(t, s, nodeA.ID, "default", "/var/lib/otherix/pools/default-on-a")
	seedBootDiskInPool(t, cli, vm.ID, srcPool.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedExplicitMigration(t, s, vm.ID, nodeA.ID, nodeB.ID, "default", true)

	// No AgentTaskID persisted: cancel while pending, source never contacted.
	if _, err := s.CancelMigration(ctx, m.ID, "operator cancelled while pending"); err != nil {
		t.Fatalf("cancel migration: %v", err)
	}

	// PollTask would error if reached - it must NOT be reached.
	agent := &fakeMigrationAgent{pollErr: errors.New("poll must not be called")}
	placer := &fakePlacer{}

	h := migrations.MigrateHandler(s, agent, placer, migrations.MigrateConfig{}, discardLogger())
	if err := h(ctx, jobArgs(t, taskID, m.ID)); err != nil {
		t.Fatalf("MigrateHandler(cancelled-pending) = %v, want nil", err)
	}

	if agent.pollCalls != 0 {
		t.Errorf("pollCalls = %d, want 0 (no AgentTaskID -> nothing to reconcile)", agent.pollCalls)
	}
	if len(agent.cancelCalls) != 0 {
		t.Errorf("cancelCalls = %v, want none (nothing moved)", agent.cancelCalls)
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusCancelled {
		t.Errorf("task status = %q, want cancelled", task.Status)
	}
	got, err := s.MigrationByID(ctx, m.ID)
	if err != nil {
		t.Fatalf("MigrationByID: %v", err)
	}
	if got.Phase != store.MigrationPhaseCancelled {
		t.Errorf("migration phase = %q, want cancelled", got.Phase)
	}
}
