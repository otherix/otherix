// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import "github.com/google/uuid"

// ReplicateArgs is the queue job-args payload for an artifact.replicate task:
// pull the blob with Digest onto TargetNodeID to add one durable replica. TaskID
// lets the worker read its own task row back for the running/finalize
// transitions.
type ReplicateArgs struct {
	TaskID       uuid.UUID `json:"task_id"`
	Digest       string    `json:"digest"`
	TargetNodeID uuid.UUID `json:"target_node_id"`
}

// Kind names the job kind, mirroring the dispatcher registration and the
// Task.type string surfaced through tasks.{list,get}.
func (ReplicateArgs) Kind() string { return "artifact.replicate" }
