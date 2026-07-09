// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// scheduledVM drives the real admission + reconcile sequence: create a pending
// VM against a ready pool, run one ScheduleFunc tick to bind it to the node, and
// return its id and name. A scheduled VM has a pinned node, so a lifecycle op
// (stop) resolves a target and enqueues a task through the guarded chokepoint.
func scheduledVM(t *testing.T, h *harness, admin string, adminID uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	_, poolName := schedulableFixtureWithNode(t, h, adminID)
	vmID := createPendingVM(t, h, admin, poolName)

	fn := vms.ScheduleFunc(h.store, vms.ScheduleConfig{Algorithm: "least_vm_count"}, scheduleLogger(), defaultScheduleResources())
	if err := fn(context.Background()); err != nil {
		t.Fatalf("ScheduleFunc: %v", err)
	}
	return vmID, vmNameOf(t, h, admin, vmID)
}

// stopTaskCount returns the number of vm.stop tasks the store holds for the VM.
func stopTaskCount(t *testing.T, h *harness, vmID uuid.UUID) int {
	t.Helper()
	typ := "vm.stop"
	tasks, err := h.store.ListTasksAny(context.Background(), store.ListTasksAnyParams{
		TypeFilter: &typ, ResourceIDFilter: &vmID, LimitCount: 100,
	})
	if err != nil {
		t.Fatalf("ListTasksAny: %v", err)
	}
	return len(tasks)
}

// postStop issues POST /v1/vms/{name}/stop, optionally carrying an
// Idempotency-Key, and returns the accepted task id. It fails the test on any
// non-202 status.
func postStop(t *testing.T, h *harness, name, admin, idempotencyKey string) string {
	t.Helper()
	var headers map[string]string
	if idempotencyKey != "" {
		headers = map[string]string{"Idempotency-Key": idempotencyKey}
	}
	resp := h.do(t, http.MethodPost, "/v1/vms/"+name+"/stop", struct{}{}, admin, headers)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202", resp.StatusCode)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	decodeJSON(t, resp, &out)
	if out.TaskID == "" {
		t.Fatal("stop returned empty task_id")
	}
	return out.TaskID
}

// forgetIdempotencyRecords wipes every middleware idempotency_keys row while
// leaving the idempotency-task INDEX (a separate subtree) intact. This is the
// exact durable state a crash leaves after EnqueueTask commits the side-effect +
// index but the request dies before CompleteIdempotencyKey: the completed record
// is absent, so a retry misses the middleware cache and re-runs the handler,
// while the index still points at the task the crashed request minted.
func forgetIdempotencyRecords(t *testing.T) {
	t.Helper()
	if _, err := sharedEtcdClient.Raw().Delete(context.Background(),
		etcd.Key("idempotency_keys")+"/", clientv3.WithPrefix()); err != nil {
		t.Fatalf("wipe idempotency_keys: %v", err)
	}
}

// TestExactlyOnce_ReclaimReplaysSameTask proves exactly-once across the real
// crash-window reclaim path end to end: a mutating request mints a task through
// the guarded EnqueueTask chokepoint, the middleware's completed record is then
// removed (simulating a crash before CompleteIdempotencyKey, followed by a
// reclaim) while the durable idempotency-task index survives, and the SAME
// request is re-issued. The second run misses the middleware cache and re-enters
// the handler, but the store guard dedupes on the surviving index: the response
// replays the original task id and no second task is created.
func TestExactlyOnce_ReclaimReplaysSameTask(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	vmID, vmName := scheduledVM(t, h, admin, adminID)

	const key = "crash-window-key-1"

	// First request: mints a vm.stop task and, via EnqueueTask, the durable
	// idempotency-task index. The middleware also writes a completed row.
	firstTask := postStop(t, h, vmName, admin, key)
	if n := stopTaskCount(t, h, vmID); n != 1 {
		t.Fatalf("after first stop, vm.stop task count = %d, want 1", n)
	}

	// Simulate the crash + reclaim: drop the middleware's completed record so the
	// retry does NOT replay from cache, but keep the idempotency-task index (the
	// crash-window state: side-effect + index committed, completed record absent).
	forgetIdempotencyRecords(t)

	// Re-issue the SAME request. It misses the middleware cache and re-runs the
	// handler; the store guard must dedupe on the surviving index.
	secondTask := postStop(t, h, vmName, admin, key)

	if secondTask != firstTask {
		t.Errorf("reclaim re-run returned task %s, want the original %s (guard must dedupe)", secondTask, firstTask)
	}
	if n := stopTaskCount(t, h, vmID); n != 1 {
		t.Errorf("after reclaim re-run, vm.stop task count = %d, want exactly 1 (guard deduped)", n)
	}
}

// TestExactlyOnce_NoKeyMintsDistinctTasks is the opt-out baseline: the guard is
// opt-in via the Idempotency-Key header. Two stops with NO key mint two distinct
// tasks, confirming the exactly-once path in the sibling test comes from the key,
// not from some unconditional dedupe.
func TestExactlyOnce_NoKeyMintsDistinctTasks(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	vmID, vmName := scheduledVM(t, h, admin, adminID)

	first := postStop(t, h, vmName, admin, "")
	second := postStop(t, h, vmName, admin, "")

	if first == second {
		t.Errorf("two key-less stops returned the same task %s, want distinct", first)
	}
	if n := stopTaskCount(t, h, vmID); n != 2 {
		t.Errorf("key-less vm.stop task count = %d, want 2 (guard is opt-in)", n)
	}
}
