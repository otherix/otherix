// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// agentClient is the narrow interface agentSnapshotExecutor depends on (Go-style
// "consumer defines"). *agentclient.Client satisfies it structurally; tests pass a
// fake without importing the production client at all.
type agentClient interface {
	PostSnapshot(ctx context.Context, endpoint, vmName string, body agentapi.SnapshotCreateRequest) (uuid.UUID, error)
	DeleteSnapshot(ctx context.Context, endpoint string, vmID uuid.UUID, snapshotName string, orphaned []string) error
	PollTask(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (agentclient.TaskTerminal, error)
}

// agentSnapshotExecutor is the production SnapshotExecutor: it translates a
// CreateExecArgs / DeleteExecArgs into the agent calls over mTLS. Create is split
// into Post (the one-shot PostSnapshot) and Poll (the resumable PollTask), so the
// worker persists the returned agent task id between them and never re-POSTs on a
// redelivery; Delete is a single best-effort agent delete of the orphaned blobs.
// Errors flow through verbatim (wrapped with phase context) so the worker's
// failTask path renders the originating layer; retry classification is the queue's
// MaxAttempts/backoff, not pre-empted here.
type agentSnapshotExecutor struct {
	client agentClient
}

// NewAgentSnapshotExecutor returns the production SnapshotExecutor that drives
// vm.snapshot.* against an agent. The constructor takes only the HTTP client:
// VMName + AdvertisedEndpoint are resolved upstream by the worker, so the executor
// needs no entity readers.
func NewAgentSnapshotExecutor(client agentClient) SnapshotExecutor {
	return &agentSnapshotExecutor{client: client}
}

// Post submits POST /v1/vms/{vm_name}/snapshots against a.AdvertisedEndpoint and
// returns the agent task id driving the capture. The worker persists this id
// (UpdateTaskAgentTaskID) before polling, so a redelivery resumes Poll instead of
// re-POSTing a second blockdev-backup.
func (e *agentSnapshotExecutor) Post(ctx context.Context, a CreateExecArgs) (uuid.UUID, error) {
	desc := a.Description
	body := agentapi.SnapshotCreateRequest{Name: a.SnapshotName}
	if desc != "" {
		body.Description = &desc
	}

	agentTaskID, err := e.client.PostSnapshot(ctx, a.AdvertisedEndpoint, a.VMName, body)
	if err != nil {
		return uuid.Nil, fmt.Errorf("post snapshot: %w", err)
	}
	return agentTaskID, nil
}

// Poll drives the agent task at agentTaskID to terminal against endpoint. A
// terminal success decodes the agent's reported manifest (disks[] +
// vm_state_at_snapshot) into CreateExecResult; failed/cancelled surface as errors
// the worker writes into tasks.error. Safe to resume across redeliveries.
func (e *agentSnapshotExecutor) Poll(ctx context.Context, endpoint string, agentTaskID uuid.UUID) (CreateExecResult, error) {
	terminal, err := e.client.PollTask(ctx, endpoint, agentTaskID)
	if err != nil {
		return CreateExecResult{}, fmt.Errorf("poll task: %w", err)
	}

	switch terminal.Status {
	case "success":
		return decodeSnapshotResult(terminal.Result)
	case "failed", "cancelled":
		return CreateExecResult{}, fmt.Errorf("agent task %s: %s", terminal.Status, formatAgentError(terminal.Error))
	default:
		return CreateExecResult{}, fmt.Errorf("agent returned unexpected terminal status %q", terminal.Status)
	}
}

// Delete removes the orphaned blobs the snapshot's manifest no longer needs.
// OrphanedBlobs is already fail-closed-filtered CP-side (only digests with zero
// remaining refs), so the agent removes exactly those and the snapshot's local
// manifest. A delete error is returned so the worker fails the task and the
// dispatcher retries (the row stays soft-deleted; orphaned blobs leak meanwhile).
func (e *agentSnapshotExecutor) Delete(ctx context.Context, a DeleteExecArgs) error {
	if err := e.client.DeleteSnapshot(ctx, a.AdvertisedEndpoint, a.VMID, a.SnapshotName, a.OrphanedBlobs); err != nil {
		return fmt.Errorf("delete snapshot: %w", err)
	}
	return nil
}

// decodeSnapshotResult is the typed projection of the agent's create-task result.
// The agent task result carries the produced Snapshot manifest; we re-marshal the
// terminal result map and unmarshal into agentapi.Snapshot rather than hand-walking
// the map[string]any so the disk array decodes cleanly.
func decodeSnapshotResult(result map[string]any) (CreateExecResult, error) {
	if result == nil {
		return CreateExecResult{}, errors.New("agent success result is nil")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return CreateExecResult{}, fmt.Errorf("re-marshal agent result: %v", err)
	}
	var snap agentapi.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return CreateExecResult{}, fmt.Errorf("decode agent Snapshot: %v", err)
	}
	if len(snap.Disks) == 0 {
		return CreateExecResult{}, errors.New("agent snapshot result has no disks")
	}

	disks := make([]store.SnapshotDisk, 0, len(snap.Disks))
	for _, d := range snap.Disks {
		// The agent is authoritative for what it captured, but its reported digest
		// becomes a reference-graph / placement-map key and later flows back out as an
		// `orphaned` delete param: a malformed digest would be a permanent bad key and
		// (without the agent-side hex guard) a path-injection sink. Reject the whole
		// result if any digest is not a 64-char lowercase hex content address.
		if !isHexSHA256Lower(d.Sha256) {
			return CreateExecResult{}, fmt.Errorf("agent reported a non-hex blob digest %q", d.Sha256)
		}
		disks = append(disks, store.SnapshotDisk{
			Index:     clampInt32(d.Index),
			Device:    d.Device,
			SHA256:    d.Sha256,
			SizeBytes: d.SizeBytes,
			Format:    string(d.Format),
		})
	}
	return CreateExecResult{
		Disks:             disks,
		VMStateAtSnapshot: store.VMStateAtSnapshot(snap.VMStateAtSnapshot),
	}, nil
}

// isHexSHA256Lower reports whether s is a 64-char lowercase hex string (a
// content-address digest). Mirrors the agent-side guard; used to reject malformed
// agent-reported digests before they become reference-graph keys.
func isHexSHA256Lower(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// clampInt32 narrows the agent-reported disk index (a small ordinal: virtio0,
// virtio1, ...) into the int32 the manifest row stores. A disk index never
// approaches int32 in practice; the bound is defensive so a malformed agent report
// cannot overflow the narrowing.
func clampInt32(v int) int32 {
	const maxInt32 = int32(1<<31 - 1)
	if v > int(maxInt32) {
		return maxInt32
	}
	if v < 0 {
		return 0
	}
	return int32(v) //nolint:gosec // bounded immediately above
}

// formatAgentError renders an *agentclient.AgentError envelope into a single line
// for embedding in the worker's error message. Nil / empty envelopes degrade to a
// parseable placeholder so the wrapping fmt.Errorf still produces a usable message.
func formatAgentError(ae *agentclient.AgentError) string {
	if ae == nil {
		return "<no error envelope>"
	}
	if ae.Code == "" && ae.Message == "" {
		return "<empty error envelope>"
	}
	if ae.Code == "" {
		return ae.Message
	}
	if ae.Message == "" {
		return ae.Code
	}
	return fmt.Sprintf("%s: %s", ae.Code, ae.Message)
}
