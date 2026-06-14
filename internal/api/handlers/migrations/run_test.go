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
	"sync"
	"testing"
	"time"

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

	// incomingReq / outgoingReq capture the last requests so a test can assert the
	// threaded identities + disk spec.
	incomingReq agentapi.MigrationIncomingRequest
	outgoingReq agentapi.MigrationOutgoingRequest

	// terminal is the staged outgoing-task outcome the worker's PollTask sees.
	terminal agentclient.TaskTerminal
	pollErr  error

	// startTargetErr / deleteSourceErr stage post-cutover convergence failures so a
	// test can prove the worker logs-and-continues (never fails the committed migration).
	startTargetErr  error
	deleteSourceErr error

	// agentTaskID is the id StartOutgoingMigration returns (and PollTask echoes).
	agentTaskID uuid.UUID
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
	defer f.mu.Unlock()
	f.pollCalls++
	if f.pollErr != nil {
		return agentclient.TaskTerminal{}, f.pollErr
	}
	return f.terminal, nil
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

// seedBootDisk writes a device-order-0 boot disk for vm via the etcd client
// (the worker reads it through ListVMDisksByVM to size the destination disk and
// fill VMSpec.Disks[0]).
func seedBootDisk(t *testing.T, cli *etcd.Client, vmID uuid.UUID, sizeGib int32) store.VMDisk {
	t.Helper()
	ctx := context.Background()
	d := store.VMDisk{
		ID: uuid.New(), VmID: vmID, StoragePoolID: uuid.New(),
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
func seedNodelessMigration(t *testing.T, s *etcdstore.Store, vmID, source uuid.UUID) (store.Migration, uuid.UUID) {
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
		Live:         true,
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

// TestRunMigration_HappyNodeless drives the full node-less success path: the
// placer picks target B, the worker binds it, runs the two-phase handshake, the
// source outgoing task converges, and CommitMigrationCutover re-pins the VM to B.
func TestRunMigration_HappyNodeless(t *testing.T) {
	s, cli := freshStore(t)
	ctx := context.Background()

	nodeA := seedReadyNode(t, s, "node-a", "https://node-a:9443")
	nodeB := seedReadyNode(t, s, "node-b", "https://node-b:9443")
	vm := seedPinnedVM(t, cli, nodeA.ID)
	seedBootDisk(t, cli, vm.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

	seedBootDisk(t, cli, vm.ID, 40)
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
	seedBootDisk(t, cli, vm.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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

func (sp *placementLockSpy) AcquirePlacementLock(ctx context.Context, lockKey int64) error {
	sp.mu.Lock()
	sp.events = append(sp.events, "lock")
	sp.lockKeys = append(sp.lockKeys, lockKey)
	sp.mu.Unlock()
	return sp.MigrationWorkerStore.AcquirePlacementLock(ctx, lockKey)
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
	seedBootDisk(t, cli, vm.ID, 40)
	seedPool(t, s, nodeB.ID, "default", "/var/lib/otherix/pools/default")
	m, taskID := seedNodelessMigration(t, s, vm.ID, nodeA.ID)

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

	if len(spy.events) < 2 || spy.events[0] != "lock" || spy.events[1] != "bind" {
		t.Errorf("call order = %v, want [lock bind ...] (placement lock held across placement -> bind)", spy.events)
	}
	if len(spy.lockKeys) == 0 || spy.lockKeys[0] != store.LockKeyPlacement {
		t.Errorf("lock key = %v, want first acquire on store.LockKeyPlacement (%d)", spy.lockKeys, store.LockKeyPlacement)
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
