// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import "github.com/google/uuid"

// NodeDrainRunArgs is the node.drain job payload. DeadlineUnix is the absolute
// wall-clock drain deadline, computed once at enqueue and redelivered verbatim
// on a control-plane restart so a resumed saga keeps the original budget.
type NodeDrainRunArgs struct {
	TaskID       uuid.UUID `json:"task_id"`
	NodeID       uuid.UUID `json:"node_id"`
	DeadlineUnix int64     `json:"deadline_unix"`
}

// Kind names the job kind.
func (NodeDrainRunArgs) Kind() string { return "node.drain" }

// DrainResult is the JSON written to the drain task's result. Migrated counts
// VMs that left the node; Remaining is what was still on it at finalize;
// InFlight is the subset of Remaining still migrating (they finish on their
// own); Stuck names the VMs with no eligible target.
type DrainResult struct {
	Code      string   `json:"code,omitempty"` // set on a non-success outcome
	Migrated  int      `json:"migrated"`
	Remaining int      `json:"remaining"`
	InFlight  int      `json:"in_flight"`
	Stuck     []string `json:"stuck,omitempty"`
}
