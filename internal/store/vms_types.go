// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
)

type CreateVMParams struct {
	ID                uuid.UUID
	OwnerID           uuid.UUID
	Name              string
	Description       string
	Architecture      CPUArch
	ImageURL          string
	ImageSHA256       []byte
	ImageFormat       ImageFormat
	CpuCores          int32
	MemoryMib         int32
	CPUModel          string
	MachineType       string
	FirmwareID        *uuid.UUID
	PinnedNodeID      *uuid.UUID
	UserData          *string
	CloudInitDisabled bool
	Labels            []byte
	// SchedulingSpec is the JSON-encoded store.SchedulingSpec captured at
	// admission; CreateUnscheduledVM persists it verbatim.
	SchedulingSpec []byte
}

type ListVMsParams struct {
	PoolIDFilter    *uuid.UUID
	NodeIDFilter    *uuid.UUID
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListVMsByOwnerParams struct {
	OwnerID         uuid.UUID
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListVMsForNodeDeclaredRow struct {
	Name         string
	DesiredPhase VMDesiredPhase
	Generation   int64
}

type UpdateVMDesiredPhaseParams struct {
	DesiredPhase VMDesiredPhase
	ID           uuid.UUID
}

// VMSchedulingStatus is the CP-side placement state of a VM.
type VMSchedulingStatus string

// VM scheduling states.
const (
	// VMSchedulingUnscheduled means the VM exists but the scheduler has not
	// yet bound it to a (node, pool); it is awaiting ready dependencies.
	VMSchedulingUnscheduled VMSchedulingStatus = "unscheduled"
	// VMSchedulingScheduled means the scheduler has bound the VM and enqueued
	// the agent create job.
	VMSchedulingScheduled VMSchedulingStatus = "scheduled"
)

// Scheduling-reason machine codes surfaced on VM.SchedulingReason and the
// public VM.status.reason. Stable, snake_case.
const (
	SchedReasonPendingSchedule       = "pending_schedule"
	SchedReasonPoolNotFound          = "pool_not_found"
	SchedReasonPoolNotReady          = "pool_not_ready"
	SchedReasonPoolNotOnNode         = "pool_not_on_node"
	SchedReasonPoolNotWritable       = "pool_not_writable"
	SchedReasonNoEligibleNodes       = "no_eligible_nodes"
	SchedReasonNodePressure          = "node_pressure"
	SchedReasonInsufficientResources = "insufficient_resources"
	SchedReasonNetworkNotReady       = "network_not_ready"
	SchedReasonNetworkNotFound       = "network_not_found"
	SchedReasonSubnetExhausted       = "subnet_exhausted"
	SchedReasonFirmwareNotReady      = "firmware_not_ready"
)

// SchedulingSpec is the deferred placement input captured at admission and
// consumed at bind. Stored as JSON in VM.SchedulingSpec so admission stays a
// pure write and the scheduler resolves the (node, pool) later.
type SchedulingSpec struct {
	PoolName    string  `json:"pool_name"`
	DiskGiB     int32   `json:"disk_gib"`
	NetworkName *string `json:"network_name,omitempty"`
	NodeHint    *string `json:"node_hint,omitempty"`
}

// VMBindWrites bundles the rows BindScheduledVM commits when the scheduler
// binds an unscheduled VM: the boot disk, an optional NIC, the create task,
// and the enqueued job args, plus the chosen node.
type VMBindWrites struct {
	PinnedNodeID uuid.UUID
	Disk         CreateVMDiskParams
	Nic          *CreateVMNicParams
	Task         CreateTaskParams
	Job          queue.JobArgs
}

// MarshalSchedulingSpec encodes a SchedulingSpec to the JSON bytes stored in
// CreateVMParams.SchedulingSpec / VM.SchedulingSpec.
func MarshalSchedulingSpec(s SchedulingSpec) ([]byte, error) { return json.Marshal(s) }

// UnmarshalSchedulingSpec decodes the JSON bytes from VM.SchedulingSpec.
func UnmarshalSchedulingSpec(b []byte) (SchedulingSpec, error) {
	var s SchedulingSpec
	if len(b) == 0 {
		return s, nil
	}
	err := json.Unmarshal(b, &s)
	return s, err
}
