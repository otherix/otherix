// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
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

	// Pinned to a node row that no longer exists, the shape a force-delete
	// leaves behind for a VM it orphaned.
	orphaned := seedPinnedVM(t, opID, uuid.New())
	if got := hbPostReportingVM(t, agentSrv.URL, reporter, orphaned.ID); len(got) != 0 {
		t.Errorf("vm_tombstones = %+v, want none (the home node row is gone)", got)
	}

	partitioned := seedPinnedVM(t, opID, unreachable.nodeID)
	if got := hbPostReportingVM(t, agentSrv.URL, reporter, partitioned.ID); len(got) != 0 {
		t.Errorf("vm_tombstones = %+v, want none (the home node is out of contact)", got)
	}
}
