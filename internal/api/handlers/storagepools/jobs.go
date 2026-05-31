// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// This file holds the queue contract for the storage_pool.scan
// task: the job-arg payload, the executor seam + its argument/result shapes, the
// terminal error-code catalogue, and the result/error marshallers. The worker
// implementation that consumes this contract lives in run.go.

// StoragePoolScanArgs is the queue job-args payload for a `storage_pool.scan`
// task. The atomic-enqueue handler inserts the row, picks a fresh task id, and
// enqueues this payload; the worker reads task / pool ids back to re-walk the
// saga it observed at hand-off.
type StoragePoolScanArgs struct {
	TaskID uuid.UUID `json:"task_id"`
	PoolID uuid.UUID `json:"pool_id"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value surfaced through
// tasks.{list,get}.
func (StoragePoolScanArgs) Kind() string { return "storage_pool.scan" }

// ScanArgs is the per-task input the executor receives. Distinct from
// StoragePoolScanArgs (the queue job args): the worker resolves pool and node
// before dispatching, so the executor sees a self-contained dependency-free
// struct.
//
// PoolName is the agent-side identifier the executor passes through
// `agentclient.PostScan` - the agent's pool registry is name-keyed. PoolID is
// preserved as the CP-side row id for logging / pressure-transition correlation.
type ScanArgs struct {
	PoolID             uuid.UUID
	PoolName           string
	NodeID             uuid.UUID
	AdvertisedEndpoint string
}

// ScanResult is the executor's output. Surfaces verbatim into storage_pools
// usage and into the task's `result` JSONB column.
type ScanResult struct {
	CapacityBytes  int64     `json:"capacity_bytes"`
	AvailableBytes int64     `json:"available_bytes"`
	ReportedAt     time.Time `json:"reported_at"`
}

// ScanExecutor is the per-task-type seam. The production implementation is
// agentScanExecutor over the agentclient package; tests use a small in-package
// fake (`fakeScanExecutor`).
type ScanExecutor interface {
	Execute(ctx context.Context, args ScanArgs) (ScanResult, error)
}

// Failure code constants for the task `error` JSONB envelope.
const (
	errCodePoolNotFound = "pool_not_found"
	errCodeNodeNotFound = "node_not_found"
	errCodeScanFailed   = "scan_failed"
)

// taskErrorJSON is the inner-error envelope written into tasks.error. Mirrors
// the public `Error.error` shape (code + message); details are not populated by
// task workers.
type taskErrorJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// marshalResult serialises a ScanResult into the JSONB payload the OpenAPI
// Task.result field surfaces. ReportedAt is normalised to UTC so the wire form
// is timezone-stable.
func marshalResult(r ScanResult) ([]byte, error) {
	r.ReportedAt = r.ReportedAt.UTC()
	return json.Marshal(r)
}

// marshalError serialises a (code, message) pair into the JSONB payload the
// OpenAPI Task.error field surfaces.
func marshalError(code, message string) ([]byte, error) {
	return json.Marshal(taskErrorJSON{Code: code, Message: message})
}
