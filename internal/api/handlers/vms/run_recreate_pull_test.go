// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/blobbroker"
	"github.com/otherix/otherix/internal/store"
)

// pullWorkerStoreStub extends the create/lifecycle worker store stub with the
// observed node-blob inventory read the pull-before-materialize seam keys on.
// inventory is the target node's observed blobs; empty means the target holds
// nothing yet (the broker must pull).
type pullWorkerStoreStub struct {
	createLifecycleWorkerStoreStub
	inventory []store.NodeBlob
}

func (s *pullWorkerStoreStub) NodeBlobInventory(context.Context, uuid.UUID) ([]store.NodeBlob, error) {
	return s.inventory, nil
}

// brokerSpy records the BrokerPull calls (digest + target node) and the order
// relative to the create executor, plus a canned error to surface (e.g. a
// no-holder ErrBlobUnavailable). pulledBeforeExec is recorded against the spy
// executor's called flag so the test can prove pull-before-materialize ordering.
type brokerSpy struct {
	err  error
	exec *spyCreateExecutor

	calls            []brokerPullCall
	pulledBeforeExec bool
}

type brokerPullCall struct {
	digest string
	node   uuid.UUID
}

func (b *brokerSpy) BrokerPull(_ context.Context, digest string, node uuid.UUID) error {
	if b.exec != nil && !b.exec.called {
		b.pulledBeforeExec = true
	}
	b.calls = append(b.calls, brokerPullCall{digest: digest, node: node})
	return b.err
}

// TestRunCreatePullsSnapshotBlobBeforeMaterialize drives the REAL create worker
// for a recreate-from-snapshot where the TARGET node does not hold the
// snapshot's blob in observed inventory. It asserts the broker pull runs with
// {digest, targetNodeID} BEFORE the create executor (pull-before-materialize),
// and that a successful pull lets the create proceed to the agent.
func TestRunCreatePullsSnapshotBlobBeforeMaterialize(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	snapID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	snap := store.Snapshot{
		ID: snapID,
		Disks: []store.SnapshotDisk{
			{Index: 0, Device: "virtio0", SHA256: "aaaa", SizeBytes: 1},
		},
	}
	st := &pullWorkerStoreStub{
		createLifecycleWorkerStoreStub: createLifecycleWorkerStoreStub{
			vm:           store.VM{ID: vmID, SourceSnapshotID: &snapID},
			node:         node,
			disk:         store.VMDisk{VmID: vmID},
			pool:         store.StoragePool{Name: "pool-a"},
			snapshotByID: func(uuid.UUID) (store.Snapshot, error) { return snap, nil },
		},
		// Target holds nothing: the broker must pull.
		inventory: nil,
	}
	exec := &spyCreateExecutor{}
	broker := &brokerSpy{exec: exec}

	err := runCreate(context.Background(), st, exec, broker, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID, SourceSnapshotID: &snapID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if len(broker.calls) != 1 {
		t.Fatalf("BrokerPull calls = %d, want 1", len(broker.calls))
	}
	if got := broker.calls[0]; got.digest != "aaaa" || got.node != nodeID {
		t.Errorf("BrokerPull(%q, %v), want (aaaa, %v)", got.digest, got.node, nodeID)
	}
	if !broker.pulledBeforeExec {
		t.Errorf("broker pull did not run BEFORE the create executor (pull-before-materialize violated)")
	}
	if !exec.called {
		t.Errorf("create executor not called after a successful pull")
	}
}

// TestRunCreateBlobUnavailableIsRetryable pins the deliberate Fix Risk Gate
// decision: a no-holder ErrBlobUnavailable from the broker is RETRYABLE, not
// terminal. Holder discovery reads OBSERVED inventory which lags the snapshot by
// one heartbeat, so a recreate issued seconds after a snapshot can legitimately
// see no holder yet; the next attempt self-heals once inventory propagates.
// Therefore the worker must NOT finalize success, must NOT call the executor,
// and must return a NON-nil error so the dispatcher re-runs the job.
func TestRunCreateBlobUnavailableIsRetryable(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	snapID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	snap := store.Snapshot{
		ID:    snapID,
		Disks: []store.SnapshotDisk{{Index: 0, Device: "virtio0", SHA256: "aaaa", SizeBytes: 1}},
	}
	st := &pullWorkerStoreStub{
		createLifecycleWorkerStoreStub: createLifecycleWorkerStoreStub{
			vm:           store.VM{ID: vmID, SourceSnapshotID: &snapID},
			node:         node,
			disk:         store.VMDisk{VmID: vmID},
			pool:         store.StoragePool{Name: "pool-a"},
			snapshotByID: func(uuid.UUID) (store.Snapshot, error) { return snap, nil },
		},
		inventory: nil,
	}
	exec := &spyCreateExecutor{}
	broker := &brokerSpy{exec: exec, err: blobbroker.ErrBlobUnavailable}

	err := runCreate(context.Background(), st, exec, broker, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID, SourceSnapshotID: &snapID}, 5*time.Minute)
	if err == nil {
		t.Fatalf("runCreate = nil on no-holder; want a NON-nil retryable error (blob_unavailable must be retryable, not terminal)")
	}
	if exec.called {
		t.Errorf("create executor called despite no-holder pull failure; must not materialize without the blob")
	}
	// Retryable: the failed envelope is written (so the operator sees progress)
	// but the cause is propagated so the dispatcher requeues. The task is NOT
	// committed terminal-success.
	if st.projectedCreate {
		t.Errorf("ProjectVMCreateSuccess called on no-holder; the create did not happen")
	}
	if !st.finalizedFailed {
		t.Errorf("failed envelope not written on no-holder")
	}
}

// TestRunCreateSkipsPullWhenTargetHoldsBlob pins the single-replica same-node fast
// path: when the target node already reports the snapshot's blob in observed
// inventory (e.g. the producing node IS the target), NO pull happens and the
// create proceeds straight to the agent. Preserves current behavior exactly.
func TestRunCreateSkipsPullWhenTargetHoldsBlob(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	snapID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	snap := store.Snapshot{
		ID:    snapID,
		Disks: []store.SnapshotDisk{{Index: 0, Device: "virtio0", SHA256: "aaaa", SizeBytes: 1}},
	}
	st := &pullWorkerStoreStub{
		createLifecycleWorkerStoreStub: createLifecycleWorkerStoreStub{
			vm:           store.VM{ID: vmID, SourceSnapshotID: &snapID},
			node:         node,
			disk:         store.VMDisk{VmID: vmID},
			pool:         store.StoragePool{Name: "pool-a"},
			snapshotByID: func(uuid.UUID) (store.Snapshot, error) { return snap, nil },
		},
		// Target already holds the blob: no pull.
		inventory: []store.NodeBlob{{Digest: "aaaa", SizeBytes: 1}},
	}
	exec := &spyCreateExecutor{}
	broker := &brokerSpy{exec: exec, err: errors.New("broker must not be called")}

	err := runCreate(context.Background(), st, exec, broker, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID, SourceSnapshotID: &snapID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if len(broker.calls) != 0 {
		t.Errorf("BrokerPull called %d times on a same-node hit; the fast path must skip the pull", len(broker.calls))
	}
	if !exec.called {
		t.Errorf("create executor not called on the same-node fast path")
	}
}

// TestRunCreateImageSourceSkipsBlobPull confirms an ordinary image-sourced
// create (no source snapshot) never touches the broker or the inventory read.
func TestRunCreateImageSourceSkipsBlobPull(t *testing.T) {
	nodeID := uuid.New()
	vmID := uuid.New()
	fresh := time.Now().Add(-5 * time.Second)
	node := store.Node{ID: nodeID, Name: "node-x", Status: store.NodeStatusReady, LastHeartbeatAt: &fresh}
	st := &pullWorkerStoreStub{
		createLifecycleWorkerStoreStub: createLifecycleWorkerStoreStub{
			vm:   store.VM{ID: vmID, ImageURL: "https://example.test/img.qcow2"},
			node: node,
			disk: store.VMDisk{VmID: vmID},
			pool: store.StoragePool{Name: "pool-a"},
		},
	}
	exec := &spyCreateExecutor{}
	broker := &brokerSpy{exec: exec, err: errors.New("broker must not be called")}

	err := runCreate(context.Background(), st, exec, broker, discardLog(),
		VMCreateArgs{TaskID: uuid.New(), VMID: vmID, NodeID: nodeID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("runCreate err = %v, want nil", err)
	}
	if len(broker.calls) != 0 {
		t.Errorf("BrokerPull called on an image-sourced create; expected zero pulls")
	}
	if !exec.called {
		t.Errorf("create executor not called on an image-sourced create")
	}
}
