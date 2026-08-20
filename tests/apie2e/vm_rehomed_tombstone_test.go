// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// TestReportingAVMThatLivesOnAnotherNodeYieldsTombstone drives the duplicate a
// re-bind under the same id leaves behind. A node force-deleted mid-bind has that
// bind rolled back and the scheduler places the VM - same id - on another node;
// when the original host is rebuilt and readmitted it replays the guest from its
// own on-node record and reports it, and two copies are live at once.
//
// The readmitted host is modelled by a second node reporting a VM pinned
// elsewhere, which is exactly the state that sequence lands in. It must be told
// to tear its copy down.
func TestReportingAVMThatLivesOnAnotherNodeYieldsTombstone(t *testing.T) {
	h := newE2E(t)
	_, opID := loginAs(t, h, auth.RoleOperator)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	home := wgSeedAgent(t, h, caCert, caKey, "node-rehomed-home")
	staleHolder := wgSeedAgent(t, h, caCert, caKey, "node-rehomed-stale")

	vm := seedPinnedVM(t, opID, home.nodeID)

	tombstones := hbPostReportingVM(t, agentSrv.URL, staleHolder, vm.ID)
	if len(tombstones) != 1 {
		t.Fatalf("vm_tombstones = %+v, want exactly one naming %s", tombstones, vm.ID)
	}
	if tombstones[0].VMID != vm.ID.String() {
		t.Errorf("tombstone vm_id = %q, want %q", tombstones[0].VMID, vm.ID)
	}
	if tombstones[0].VMName != vm.Name {
		t.Errorf("tombstone vm_name = %q, want %q", tombstones[0].VMName, vm.Name)
	}
}

// TestReportingAVMWithNoLiveHomeYieldsNoTombstone is the safety half, driven
// against the real store because the whole guard rests on how it answers for a
// home that is gone or out of contact.
//
// A node force-deleted while it was RUNNING VMs leaves them pinned to the
// now-deleted node row; when that host returns it holds the ONLY copy of each
// guest. The same goes for a home the control plane cannot currently reach: it
// may be dead. Neither may be ordered to destroy what it has.
func TestReportingAVMWithNoLiveHomeYieldsNoTombstone(t *testing.T) {
	h := newE2E(t)
	_, opID := loginAs(t, h, auth.RoleOperator)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	reporter := wgSeedAgent(t, h, caCert, caKey, "node-rehomed-reporter")
	unreachable := wgSeedAgentWithStatus(t, h, caCert, caKey,
		"node-rehomed-unreachable", store.NodeStatusUnreachable)

	// The force-deleted home, driven through the real DeleteNode rather than a
	// node id that never existed: a force-delete SOFT-deletes the row and leaves
	// its status untouched, so the whole safety property rests on NodeByID
	// hiding soft-deleted rows. Seeding an absent id would exercise a different
	// branch and leave that one uncovered.
	// The VM is observed running there first, so the force-delete ORPHANS it
	// rather than rolling the bind back - the arm that leaves the pin in place.
	forceDeleted := wgSeedAgent(t, h, caCert, caKey, "node-rehomed-force-deleted")
	orphaned := seedPinnedVM(t, opID, forceDeleted.nodeID)
	markVMRunning(t, h, orphaned.ID, forceDeleted.nodeID)
	if _, err := h.store.DeleteNode(context.Background(), forceDeleted.nodeID, true, opID); err != nil {
		t.Fatalf("DeleteNode(force): %v", err)
	}
	if vm, err := h.store.VMByID(context.Background(), orphaned.ID); err != nil {
		t.Fatalf("VMByID after force-delete: %v", err)
	} else if vm.PinnedNodeID == nil || *vm.PinnedNodeID != forceDeleted.nodeID {
		t.Fatalf("pin after force-delete = %v, want it still on the deleted node %v "+
			"(the orphan arm must leave it; otherwise this case no longer covers what it claims)",
			vm.PinnedNodeID, forceDeleted.nodeID)
	}
	if got := hbPostReportingVM(t, agentSrv.URL, reporter, orphaned.ID); len(got) != 0 {
		t.Errorf("vm_tombstones = %+v, want none (the home node row is soft-deleted)", got)
	}

	partitioned := seedPinnedVM(t, opID, unreachable.nodeID)
	if got := hbPostReportingVM(t, agentSrv.URL, reporter, partitioned.ID); len(got) != 0 {
		t.Errorf("vm_tombstones = %+v, want none (the home node is out of contact)", got)
	}
}

// TestReportingAVMFromAFailedMigrationYieldsNoTombstone drives the case the pin
// alone cannot decide. A migration that ends without a cutover leaves the pin at
// the source, so its target is indistinguishable from a stale duplicate by pin
// and node status alone - and yet it may hold the resumed guest, or the only
// surviving destination disk after the source tore itself down. The migration
// workers refuse to reap that state precisely because either could be the last
// copy; this signal must not reap it behind their back.
func TestReportingAVMFromAFailedMigrationYieldsNoTombstone(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, opID := loginAs(t, h, auth.RoleOperator)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	source := wgSeedAgent(t, h, caCert, caKey, "node-failedmig-source")
	target := wgSeedAgent(t, h, caCert, caKey, "node-failedmig-target")

	vm := seedPinnedVM(t, opID, source.nodeID)

	src, tgt := source.nodeID, target.nodeID
	migID, taskID := uuid.New(), uuid.New()
	if _, err := h.store.CreateMigration(ctx, store.CreateMigrationParams{
		ID: migID, VmID: vm.ID, SourceNodeID: &src, TargetNodeID: &tgt,
		Reason: store.MigrationReasonManual, Live: true,
		Task: store.CreateTaskParams{
			ID: taskID, Type: "vm.migrate", Status: store.TaskStatusPending,
			ResourceType: "migration", MaxAttempts: 3,
		},
	}, rehomedJobArgsStub{}); err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	failed := store.MigrationPhaseFailed
	if err := h.store.UpdateMigrationProgress(ctx, migID,
		store.MigrationProgressUpdate{Phase: &failed}); err != nil {
		t.Fatalf("UpdateMigrationProgress(failed): %v", err)
	}

	// The source is up and ready and the pin still names it, so every other arm
	// of the decision says "duplicate". Only the migration record says otherwise.
	if got := hbPostReportingVM(t, agentSrv.URL, target, vm.ID); len(got) != 0 {
		t.Errorf("vm_tombstones = %+v, want none (a migration tried to land this VM here)", got)
	}
}

// rehomedJobArgsStub satisfies the job-args payload CreateMigration enqueues
// with; these tests never run the job.
type rehomedJobArgsStub struct{}

func (rehomedJobArgsStub) Kind() string { return "vm.migrate" }
