// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// vmDeleteExecutorSpy records every agent dispatch the vm.delete worker makes.
// The tombstone path exists precisely because the worker can commit a delete
// WITHOUT dispatching, so "was the executor entered at all" is the assertion
// that identifies which path the worker took.
type vmDeleteExecutorSpy struct {
	calls []uuid.UUID
}

func (s *vmDeleteExecutorSpy) Execute(_ context.Context, args vms.DeleteArgs) (vms.DeleteResult, error) {
	s.calls = append(s.calls, args.VMID)
	return vms.DeleteResult{VMID: args.VMID.String()}, nil
}

// tombstoneStaleGrace is the heartbeat-staleness window the worker handlers are
// built with in these tests, matching the production default.
const tombstoneStaleGrace = 5 * time.Minute

// seedPinnedVM writes a scheduled VM pinned to nodeID, plus the pinned-node
// index the node-delete evacuation reads. It is the shape a committed bind
// lands: a VM the scheduler has placed and an agent may already be running.
func seedPinnedVM(t *testing.T, ownerID, nodeID uuid.UUID) store.VM {
	t.Helper()
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "pool-tombstone"})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	pinned := nodeID
	now := time.Now().UTC()
	vm := store.VM{
		ID: uuid.New(), OwnerID: ownerID, Name: "tombstone-vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingScheduled, SchedulingSpec: spec,
		PinnedNodeID: &pinned,
		CpuCores:     2, MemoryMib: 2048,
		Labels: []byte(`{}`), ImageFormat: store.ImageFormatQcow2,
		CreatedAt: now, UpdatedAt: now,
	}
	seedVMRow(t, vm)
	if err := sharedEtcdClient.Put(context.Background(),
		etcd.Key("index", "vms", "pinned_node", nodeID.String(), vm.ID.String()),
		[]byte(vm.ID.String())); err != nil {
		t.Fatalf("seed pinned-node index: %v", err)
	}
	return vm
}

// ageNodeHeartbeat rewrites the node's last_heartbeat_at to age, which is how a
// node that stopped answering looks to the worker's staleness check.
func ageNodeHeartbeat(t *testing.T, s *etcdstore.Store, nodeID uuid.UUID, age time.Time) {
	t.Helper()
	ctx := context.Background()
	n, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	n.LastHeartbeatAt = &age
	if err := sharedEtcdClient.PutJSON(ctx, etcd.Key("nodes", nodeID.String()), n); err != nil {
		t.Fatalf("age node heartbeat: %v", err)
	}
}

// pendingJobArgs returns the marshalled job payload the API handler enqueued for
// kind. Driving the worker with these bytes keeps the producer real: the test
// never hand-builds the args struct the dispatcher would deliver.
func pendingJobArgs(t *testing.T, s *etcdstore.Store, kind string) []byte {
	t.Helper()
	jobs, err := s.PendingJobs(context.Background())
	if err != nil {
		t.Fatalf("PendingJobs: %v", err)
	}
	for _, j := range jobs {
		if j.Kind == kind {
			return j.Args
		}
	}
	t.Fatalf("no pending %s job among %d pending jobs", kind, len(jobs))
	return nil
}

// hbPostReportingVM posts a heartbeat over mTLS in which the node reports vmID
// as a running guest, and returns the decoded tombstone list from the response.
func hbPostReportingVM(t *testing.T, baseURL string, ag wgAgent, vmID uuid.UUID) []struct {
	VMID   string `json:"vm_id"`
	VMName string `json:"vm_name"`
} {
	t.Helper()
	body := map[string]any{
		"agent_version": "test-0.1.0",
		"architecture":  "amd64",
		"capabilities": map[string]any{
			"cpu_model":        "test-cpu",
			"cpu_flags":        []string{},
			"cpu_cores_total":  4,
			"memory_total_mib": 8192,
			"kernel_version":   "test",
			"qemu_version":     "test",
		},
		"resources": map[string]any{
			"cpu_cores_available":  4,
			"memory_available_mib": 8000,
		},
		"vms": []any{map[string]any{
			"vm_uuid": vmID.String(),
			"phase":   "running",
		}},
		"networks": []any{},
		"pools":    []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL+"/v1/nodes/"+ag.name+"/heartbeat", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new heartbeat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ag.client.Do(req)
	if err != nil {
		t.Fatalf("heartbeat Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("heartbeat status = %d, want 200; body=%s", resp.StatusCode, string(b))
	}
	var out struct {
		VMTombstones []struct {
			VMID   string `json:"vm_id"`
			VMName string `json:"vm_name"`
		} `json:"vm_tombstones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode heartbeat response: %v", err)
	}
	return out.VMTombstones
}

// TestVMDeleteOnStaleNode_YieldsTombstoneOnNextHeartbeat drives the real
// cross-component sequence the teardown signal exists for: the vm.delete worker
// commits the control-plane delete WITHOUT agent confirmation because the owning
// node's heartbeat is stale, and the node that still holds the guest must then
// be told to tear it down the next time it reports the VM.
//
//  1. a VM is placed on a node,
//  2. the node's last_heartbeat_at ages past the worker's stale grace,
//  3. the vm.delete worker runs through its real dispatcher entry point on the
//     real enqueued job payload, and takes the skip-the-agent path,
//  4. the node heartbeats, still reporting that VM,
//  5. the response carries a tombstone naming it.
//
// Entering through DeleteHandler rather than calling the projection directly is
// load-bearing: WHICH path the worker takes is the whole condition the signal
// depends on, and a test that projects by hand cannot observe it.
func TestVMDeleteOnStaleNode_YieldsTombstoneOnNextHeartbeat(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	opToken, opID := loginAs(t, h, auth.RoleOperator)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-holds-a-deleted-vm")

	vm := seedPinnedVM(t, opID, ag.nodeID)

	// The node last answered an hour ago: well past the worker's stale grace, so
	// it is treated as beyond any agent teardown.
	ageNodeHeartbeat(t, h.store, ag.nodeID, time.Now().Add(-time.Hour))

	resp := h.delete(t, "/v1/vms/"+vm.Name, opToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("vm delete status = %d, want 202; body=%s", resp.StatusCode, string(b))
	}
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	taskID, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("parse task_id %q: %v", accepted.TaskID, err)
	}

	spy := &vmDeleteExecutorSpy{}
	handler := vms.DeleteHandler(h.store, spy, scheduleLogger(), tombstoneStaleGrace)
	if err := handler(ctx, pendingJobArgs(t, h.store, "vm.delete")); err != nil {
		t.Fatalf("vm.delete handler: %v", err)
	}

	// The skip-the-agent path is the precondition for the whole signal: the
	// worker committed a delete no agent ever confirmed.
	if len(spy.calls) != 0 {
		t.Fatalf("worker dispatched %d agent teardowns, want 0 (the stale node must not be contacted)", len(spy.calls))
	}
	task, err := h.store.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Fatalf("delete task status = %q, want success", task.Status)
	}
	var result struct {
		SkippedAgent bool `json:"skipped_agent"`
	}
	if err := json.Unmarshal(task.Result, &result); err != nil {
		t.Fatalf("unmarshal task result %s: %v", task.Result, err)
	}
	if !result.SkippedAgent {
		t.Fatalf("delete task result = %s, want skipped_agent=true", task.Result)
	}

	// The node comes back and still reports the guest. That report is the only
	// evidence the control plane has that the VM survived its delete.
	tombstones := hbPostReportingVM(t, agentSrv.URL, ag, vm.ID)
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

// TestDeletedVMRowSurvivesAsSoftDeleted pins the invariant the teardown signal
// rests on: ProjectVMDeleteSuccess leaves the vms row readable with a deletion
// stamp rather than removing it. A change that hard-deletes or retention-sweeps
// VM rows would silently turn the trigger from "explicitly deleted" into
// "absent", and absence is exactly the fail-open input a destructive action must
// never key on - so it must break here, visibly.
func TestDeletedVMRowSurvivesAsSoftDeleted(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, opID := loginAs(t, h, auth.RoleOperator)

	node := createReadyNode(t, h.store, "holder")
	vm := seedPinnedVM(t, opID, node.ID)

	taskID := uuid.New()
	resID := vm.ID
	if _, err := h.store.EnqueueTask(ctx, store.CreateTaskParams{
		ID: taskID, Type: "vm.delete", Status: store.TaskStatusPending,
		ResourceType: "vm", ResourceID: &resID, Args: []byte(`{}`), MaxAttempts: 25,
	}, vms.VMDeleteArgs{TaskID: taskID, VMID: vm.ID, NodeID: node.ID}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	if err := h.store.ProjectVMDeleteSuccess(ctx, vm, store.UpdateTaskFinalizedParams{
		ID: taskID, Status: store.TaskStatusSuccess, Result: []byte(`{}`),
	}); err != nil {
		t.Fatalf("ProjectVMDeleteSuccess: %v", err)
	}

	// Invisible to every ordinary reader...
	if _, err := h.store.VMByID(ctx, vm.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByID after delete = %v, want store.ErrNotFound", err)
	}
	// ...but still there, still stamped, still nameable.
	deleted, name, err := h.store.VMSoftDeleted(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMSoftDeleted: %v", err)
	}
	if !deleted || name != vm.Name {
		t.Errorf("VMSoftDeleted(%s) = (%v, %q), want (true, %q): the delete must leave a readable deletion stamp",
			vm.ID, deleted, name, vm.Name)
	}
}

// TestDeleteUnscheduledVMRefusesAScheduledVM pins the scheduling gate:
// DeleteUnscheduledVM handles only a VM that is still unscheduled, and refuses a
// scheduled one so the delete falls back to the async agent path that tears the
// guest down. Both outcomes now leave a soft-deleted row - the durable teardown
// signal - so the gate is about which delete path runs, not about whether the
// row survives.
func TestDeleteUnscheduledVMRefusesAScheduledVM(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, opID := loginAs(t, h, auth.RoleOperator)

	node := createReadyNode(t, h.store, "scheduled")
	vm := seedPinnedVM(t, opID, node.ID)

	if err := h.store.DeleteUnscheduledVM(ctx, vm.ID); !errors.Is(err, store.ErrVMNotUnscheduled) {
		t.Fatalf("DeleteUnscheduledVM(scheduled vm) = %v, want store.ErrVMNotUnscheduled", err)
	}
	if _, err := h.store.VMByID(ctx, vm.ID); err != nil {
		t.Errorf("VMByID after refused hard delete = %v, want the row intact", err)
	}

	// The gate's other side: an unscheduled VM is accepted, and it too leaves a
	// stamped row behind, so a guest that did reach an agent can still be named.
	vm.SchedulingStatus = store.VMSchedulingUnscheduled
	vm.PinnedNodeID = nil
	seedVMRow(t, vm)
	if err := h.store.DeleteUnscheduledVM(ctx, vm.ID); err != nil {
		t.Fatalf("DeleteUnscheduledVM(unscheduled vm) = %v, want nil", err)
	}
	deleted, _, err := h.store.VMSoftDeleted(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMSoftDeleted: %v", err)
	}
	if !deleted {
		t.Errorf("VMSoftDeleted after the unscheduled delete = false, want true (the teardown signal must survive)")
	}
}

// TestRollbackToUnscheduledKeepsTheTeardownSignal drives the sequence that used
// to destroy it. `node delete --force` returns a pinned-but-unobserved VM to
// unscheduled, aborting only when its create task is running - so a VM whose
// agent-side create reached the agent but whose result never got projected comes
// back as unscheduled. Deleting it then took the unscheduled path, which
// hard-deleted the row, and with the row went the only thing a teardown signal
// can be built from: the guest kept running on a node the control plane no
// longer knew about, unreachable by any mechanism.
//
// The delete now stamps the row instead, so the signal survives the round trip.
// This was previously a TestKnownGap_ test asserting the opposite; its doc also
// claimed closing the gap would change name-reuse semantics, which is wrong -
// the stamping delete drops the name guard exactly as ProjectVMDeleteSuccess
// does, so the name is free either way.
func TestRollbackToUnscheduledKeepsTheTeardownSignal(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	_, opID := loginAs(t, h, auth.RoleOperator)

	node := createReadyNode(t, h.store, "doomed")
	vm := seedPinnedVM(t, opID, node.ID)

	// The create reached the agent and materialised the guest, but the result
	// never landed: the task is failed-awaiting-retry, and no runtime row exists.
	// Only a *running* create task aborts the rollback, so this one does not.
	createTaskID := uuid.New()
	resID := vm.ID
	if _, err := h.store.EnqueueTask(ctx, store.CreateTaskParams{
		ID: createTaskID, Type: "vm.create", Status: store.TaskStatusFailed,
		ResourceType: "vm", ResourceID: &resID, Args: []byte(`{}`), MaxAttempts: 25,
	}, vms.VMCreateArgs{TaskID: createTaskID, VMID: vm.ID, NodeID: node.ID}); err != nil {
		t.Fatalf("EnqueueTask(vm.create): %v", err)
	}

	outcome, err := h.store.DeleteNode(ctx, node.ID, true, opID)
	if err != nil {
		t.Fatalf("DeleteNode(force): %v", err)
	}
	if outcome.VMsRolledBack != 1 {
		t.Fatalf("VMsRolledBack = %d, want 1", outcome.VMsRolledBack)
	}

	rolled, err := h.store.VMByID(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMByID after force delete: %v", err)
	}
	if rolled.SchedulingStatus != store.VMSchedulingUnscheduled || rolled.PinnedNodeID != nil {
		t.Fatalf("after rollback: scheduling_status=%q pinned_node_id=%v, want unscheduled and unpinned",
			rolled.SchedulingStatus, rolled.PinnedNodeID)
	}

	// Unscheduled again, so the delete takes the unscheduled path.
	if err := h.store.DeleteUnscheduledVM(ctx, vm.ID); err != nil {
		t.Fatalf("DeleteUnscheduledVM after rollback = %v, want nil", err)
	}
	deleted, _, err := h.store.VMSoftDeleted(ctx, vm.ID)
	if err != nil {
		t.Fatalf("VMSoftDeleted: %v", err)
	}
	if !deleted {
		t.Fatalf("VMSoftDeleted = false, want true: the row must keep a deletion stamp, or a guest for this id can never be named")
	}
	// The row is invisible to every reader, so the name is free and the VM 404s,
	// while the stamp remains readable by the tombstone path.
	if _, err := h.store.VMByID(ctx, vm.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByID after delete = %v, want ErrNotFound", err)
	}
}
