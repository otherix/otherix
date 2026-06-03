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

	"github.com/google/uuid"

	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// panicDeleteExecutor is a DeleteExecutor that fails the test if invoked. It
// proves runDelete never reaches the agent POST when the owning node is dead.
type panicDeleteExecutor struct{ t *testing.T }

func (p *panicDeleteExecutor) Execute(_ context.Context, _ vmshandlers.DeleteArgs) (vmshandlers.DeleteResult, error) {
	p.t.Helper()
	p.t.Fatal("agent Execute must not be called when the owning node is not serviceable")
	return vmshandlers.DeleteResult{}, nil
}

// TestVMDeleteRunHandlerDeadNodeSkipsAgent proves that for a force-deleted
// (ErrNotFound), gone, or unreachable owning node the delete handler skips the
// agent POST and projects the delete directly - reclaiming the VM and its NIC
// index entry so the referenced network unblocks (N3 R2).
func TestVMDeleteRunHandlerDeadNodeSkipsAgent(t *testing.T) {
	cases := []struct {
		name string
		// kill mutates the node so NodeByID returns ErrNotFound (force-delete)
		// or a live row in a non-serviceable status.
		kill func(t *testing.T, s *etcdstore.Store, cli *etcd.Client, nodeID uuid.UUID)
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
			name: "unreachable node",
			kill: func(t *testing.T, s *etcdstore.Store, cli *etcd.Client, nodeID uuid.UUID) {
				t.Helper()
				setNodeStatus(t, s, cli, nodeID, store.NodeStatusUnreachable)
			},
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
			h := vmshandlers.DeleteHandler(s, &panicDeleteExecutor{t: t}, log)
			if err := h(ctx, raw); err != nil {
				t.Fatalf("delete handler = %v, want nil (direct projection)", err)
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
	nodeID, poolID, templateID, _ := schedulingFixture(t, s)
	owner := uuid.New()
	name := "vm-" + uuid.NewString()[:8]

	netID = uuid.New()
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: uniqueNetName("deadnode"), Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	writes := vmCreateWrites(t, name, owner, nodeID, poolID, templateID)
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
		templateID,
		store.UpdateTaskFinalizedParams{ID: createTask, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectVMCreateSuccess: %v", err)
	}

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
