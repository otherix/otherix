// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"testing"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/store"
)

// TestNodeDeleteCascadeOrdering pins the REL-1 invariant: the node soft-delete
// ops (nodePut + name-guard delete) must be the LAST ops in the cascade, and the
// WireGuard purge ops (record + pubkey guard) must appear strictly BEFORE them.
//
// commitInChunks splits the cascade into <=120-op atomic chunks. A crash between
// chunks must leave a clean prefix that a retry re-derives. If the WG purge ever
// trailed the node-delete, a chunk boundary plus a crash would soft-delete the
// node (making the retry short-circuit at NodeByID -> ErrNotFound) while leaking
// the agent_wireguard record + pubkey guard, which no reaper ever cleans up.
//
// Revert-to-confirm: move the wgRec append block AFTER the nodePut/name-guard
// append in nodeDeleteCascade and this test goes red.
func TestNodeDeleteCascadeOrdering(t *testing.T) {
	nodeID := uuid.New()
	const nodeName = "node-a"
	const pubkey = "test-pubkey"
	wgRec := &store.AgentWireguard{NodeID: nodeID, PublicKey: pubkey}

	// A handful of cancel + orphan ops to stand in for the pre-node-delete work.
	cancelOps := []clientv3.Op{
		clientv3.OpPut("cancel-0", "v"),
		clientv3.OpPut("cancel-1", "v"),
	}
	orphanOps := []clientv3.Op{
		clientv3.OpPut("orphan-0", "v"),
	}
	membershipOps := []clientv3.Op{
		clientv3.OpDelete("membership-0"),
	}

	cascade := nodeDeleteCascade(nodeID, nodeName, "node-val", cancelOps, orphanOps, nil, membershipOps, wgRec)

	if len(cascade) == 0 {
		t.Fatalf("nodeDeleteCascade returned an empty cascade")
	}

	// The very last op must be the node soft-delete OpPut targeting nodeKey(id).
	wantLast := nodeKey(nodeID)
	gotLast := string(cascade[len(cascade)-1].KeyBytes())
	if gotLast != wantLast {
		t.Errorf("cascade last op key = %q, want %q (node soft-delete must be last)", gotLast, wantLast)
	}

	idxOf := func(key string) int {
		for i, op := range cascade {
			if string(op.KeyBytes()) == key {
				return i
			}
		}
		return -1
	}

	nodePutIdx := idxOf(nodeKey(nodeID))
	wgKeyIdx := idxOf(agentWireguardKey(nodeID))
	wgGuardIdx := idxOf(agentWireguardPubkeyGuard(pubkey))
	membershipIdx := idxOf("membership-0")

	if wgKeyIdx < 0 {
		t.Fatalf("cascade missing agentWireguardKey delete op")
	}
	if wgGuardIdx < 0 {
		t.Fatalf("cascade missing agentWireguardPubkeyGuard delete op")
	}
	if membershipIdx < 0 {
		t.Fatalf("cascade missing gateway membership delete op")
	}
	if wgKeyIdx >= nodePutIdx {
		t.Errorf("agentWireguardKey op at index %d, want before node soft-delete at index %d", wgKeyIdx, nodePutIdx)
	}
	if wgGuardIdx >= nodePutIdx {
		t.Errorf("agentWireguardPubkeyGuard op at index %d, want before node soft-delete at index %d", wgGuardIdx, nodePutIdx)
	}
	if membershipIdx >= nodePutIdx {
		t.Errorf("gateway membership op at index %d, want before node soft-delete at index %d", membershipIdx, nodePutIdx)
	}
}

// TestNodeDeleteCascadeNoWireguard pins that a node with no WG record (the
// AgentWireguardByNodeID -> ErrNotFound branch passes wgRec=nil) yields a cascade
// whose last op is still the node soft-delete and which carries no WG ops.
func TestNodeDeleteCascadeNoWireguard(t *testing.T) {
	nodeID := uuid.New()
	const nodeName = "node-b"

	cascade := nodeDeleteCascade(nodeID, nodeName, "node-val", nil, nil, nil, nil, nil)

	if len(cascade) != 2 {
		t.Fatalf("nodeDeleteCascade with no wg/cancel/orphan ops = %d ops, want 2", len(cascade))
	}
	wantLast := nodeKey(nodeID)
	if gotLast := string(cascade[len(cascade)-1].KeyBytes()); gotLast != wantLast {
		t.Errorf("cascade last op key = %q, want %q", gotLast, wantLast)
	}
	for _, op := range cascade {
		if string(op.KeyBytes()) == agentWireguardKey(nodeID) {
			t.Errorf("cascade unexpectedly contains a WireGuard op when wgRec is nil")
		}
	}
}
