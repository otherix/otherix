// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// panicDeleteExecutor is a DeleteExecutor that fails the test if invoked. It
// proves runDelete never reaches the agent POST for a terminally-dead owning
// node (force-deleted or gone).
type panicDeleteExecutor struct{ t *testing.T }

func (p *panicDeleteExecutor) Execute(_ context.Context, _ vmshandlers.DeleteArgs) (vmshandlers.DeleteResult, error) {
	p.t.Helper()
	p.t.Fatal("agent Execute must not be called when the owning node is terminally dead")
	return vmshandlers.DeleteResult{}, nil
}

// failDeleteExecutor is a DeleteExecutor that records it was called and returns
// an error - modelling a genuinely-down agent. It proves runDelete attempts a
// best-effort agent teardown for an unreachable node, then falls back to a
// direct projection when that teardown fails.
type failDeleteExecutor struct{ called bool }

func (e *failDeleteExecutor) Execute(_ context.Context, _ vmshandlers.DeleteArgs) (vmshandlers.DeleteResult, error) {
	e.called = true
	return vmshandlers.DeleteResult{}, errors.New("dial agent: connection refused")
}

// TestVMDeleteRunHandlerDeadNodeReclaims proves that for a force-deleted
// (ErrNotFound), gone, or unreachable owning node the delete handler still
// reclaims the VM and its NIC index entry so the referenced network unblocks
// (N3 R2). A terminally-dead node (force-deleted / gone) skips the agent
// outright; an unreachable node is given a best-effort agent teardown first
// (so qemu is reaped if the partition has healed) and only falls back to a
// direct projection when that teardown fails.
func TestVMDeleteRunHandlerDeadNodeReclaims(t *testing.T) {
	cases := []struct {
		name string
		// kill mutates the node so NodeByID returns ErrNotFound (force-delete)
		// or a live row in a non-serviceable status.
		kill func(t *testing.T, s *etcdstore.Store, cli *etcd.Client, nodeID uuid.UUID)
		// wantAgentAttempt is true when runDelete must attempt the agent
		// teardown before projecting the delete directly.
		wantAgentAttempt bool
	}{
		{
			name: "force-deleted node (ErrNotFound)",
			kill: func(t *testing.T, s *etcdstore.Store, _ *etcd.Client, nodeID uuid.UUID) {
				t.Helper()
				if _, err := s.DeleteNode(context.Background(), nodeID, true, uuid.New()); err != nil {
					t.Fatalf("DeleteNode(force): %v", err)
				}
			},
		},
		{
			name: "gone node",
			kill: func(t *testing.T, s *etcdstore.Store, cli *etcd.Client, nodeID uuid.UUID) {
				t.Helper()
				setNodeStatus(t, s, cli, nodeID, store.NodeStatusGone)
			},
		},
		{
			name: "unreachable node falls back after best-effort agent teardown",
			kill: func(t *testing.T, s *etcdstore.Store, cli *etcd.Client, nodeID uuid.UUID) {
				t.Helper()
				setNodeStatus(t, s, cli, nodeID, store.NodeStatusUnreachable)
			},
			wantAgentAttempt: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cli := startStore(t)
			ctx := context.Background()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			vmID, nodeID, netID, delTaskID := seedRunningVMWithNic(t, s)
			tc.kill(t, s, cli, nodeID)

			raw, _ := json.Marshal(vmshandlers.VMDeleteArgs{TaskID: delTaskID, VMID: vmID, NodeID: nodeID})
			var exec vmshandlers.DeleteExecutor
			fail := &failDeleteExecutor{}
			if tc.wantAgentAttempt {
				exec = fail
			} else {
				exec = &panicDeleteExecutor{t: t}
			}
			h := vmshandlers.DeleteHandler(s, exec, log, 5*time.Minute)
			if err := h(ctx, raw); err != nil {
				t.Fatalf("delete handler = %v, want nil (direct projection)", err)
			}
			if tc.wantAgentAttempt && !fail.called {
				t.Errorf("agent teardown not attempted for unreachable node, want best-effort attempt")
			}

			task, err := s.TaskByID(ctx, delTaskID)
			if err != nil || task.Status != store.TaskStatusSuccess {
				t.Errorf("delete task = (%+v, %v), want success", task, err)
			}
			if _, err := s.VMByID(ctx, vmID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("vm still present after delete = %v, want ErrNotFound", err)
			}
			nics, err := s.ListVMNicsByVM(ctx, vmID)
			if err != nil {
				t.Fatalf("ListVMNicsByVM: %v", err)
			}
			if len(nics) != 0 {
				t.Errorf("nics after delete = %d, want 0", len(nics))
			}
			// The NIC index entry is gone, so the network is now deletable.
			if err := s.DeleteNetwork(ctx, netID); err != nil {
				t.Errorf("DeleteNetwork after delete = %v, want nil (block must clear)", err)
			}
		})
	}
}

// seedRunningVMWithNic seeds a running VM with a single NIC on a fresh network
// plus a pending vm.delete task, returning the ids the delete handler keys off.
func seedRunningVMWithNic(t *testing.T, s *etcdstore.Store) (vmID, nodeID, netID, delTaskID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	nodeID, poolID, _ := schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]

	netID = uuid.New()
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: uniqueNetName("deadnode"), Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	writes := vmCreateWrites(t, name, owner, nodeID, poolID)
	writes.Nic = &store.CreateVMNicParams{
		ID: uuid.New(), VmID: writes.VM.ID, NetworkID: netID, DeviceOrder: 0,
		Model: store.NicModelVirtio, MacAddress: mustMAC(t, "52:54:00:de:ad:00"),
	}
	createTask, err := s.CreateScheduledVM(ctx, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return writes, nil
	})
	if err != nil {
		t.Fatalf("CreateScheduledVM: %v", err)
	}
	if err := s.ProjectVMCreateSuccess(ctx,
		store.UpsertVMRuntimeParams{VmID: writes.VM.ID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1},
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
		nil,
	); err != nil {
		t.Fatalf("ProjectVMCreateSuccess: %v", err)
	}
	// A node hosting a running VM has heartbeated recently. Bump it fresh BEFORE
	// the test's kill runs: the unreachable kill preserves last_heartbeat_at, so
	// the node stays non-stale and the agent-attempt branch survives; the gone /
	// force-deleted kills stay terminal via their own arms regardless.
	bumpHeartbeat(t, s, nodeID)

	delTask := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, delTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(delete): %v", err)
	}
	return writes.VM.ID, nodeID, netID, delTask.ID
}

// setNodeStatus rewrites the node row's status field directly so the test can
// land a live row in a non-serviceable status (gone / unreachable).
func setNodeStatus(t *testing.T, s *etcdstore.Store, cli *etcd.Client, nodeID uuid.UUID, status store.NodeStatus) {
	t.Helper()
	ctx := context.Background()
	n, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	n.Status = status
	if err := cli.PutJSON(ctx, etcd.Key("nodes", nodeID.String()), n); err != nil {
		t.Fatalf("put node %s=%s: %v", nodeID, status, err)
	}
}
