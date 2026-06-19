// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"

	"github.com/google/uuid"
)

// ReclaimArgs is the queue job-args payload for an artifact.reclaim task: delete
// the blob with Digest from TargetNodeID because the cluster holds more copies
// than it needs (an orphaned blob, or a copy beyond the desired replica count).
// TaskID lets the worker read its own task row back for the running/finalize
// transitions.
type ReclaimArgs struct {
	TaskID       uuid.UUID `json:"task_id"`
	Digest       string    `json:"digest"`
	TargetNodeID uuid.UUID `json:"target_node_id"`
}

// Kind names the job kind, mirroring the dispatcher registration and the
// Task.type string surfaced through tasks.{list,get}.
func (ReclaimArgs) Kind() string { return "artifact.reclaim" }

// Reclaimer deletes one blob copy from one node. The production implementation
// (wired in cmd/api) resolves the node's advertised endpoint and calls the agent
// reclaim endpoint; the interface keeps this package free of an agentclient
// import. A reclaim of an absent blob is a no-op success on the agent side.
type Reclaimer interface {
	Reclaim(ctx context.Context, targetNodeID uuid.UUID, digest string) error
}
