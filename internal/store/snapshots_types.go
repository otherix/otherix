// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// SnapshotDisk is one content-addressed disk blob in a snapshot manifest.
type SnapshotDisk struct {
	Index     int32  `json:"index"`
	Device    string `json:"device"` // virtio<i>
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Format    string `json:"format"` // qcow2
}

// CreateSnapshotParams is the input to Store.CreateSnapshot. The Task field
// carries the backing async task enqueued atomically with the snapshot row.
// SourceArchitecture and SourceFirmwareID capture the source VM's architecture
// and firmware at snapshot time so the manifest is self-describing for
// recreate-from-snapshot (a qcow2 blob is architecture-agnostic but a recreated
// VM is not).
type CreateSnapshotParams struct {
	ID                 uuid.UUID
	VmID               uuid.UUID
	OwnerID            uuid.UUID
	Name               string
	Description        string
	WithMemory         bool
	VMStateAtSnapshot  VMStateAtSnapshot
	SourceArchitecture CPUArch
	SourceFirmwareID   *uuid.UUID
	Task               CreateTaskParams
}

type ListSnapshotsParams struct {
	VmID            *uuid.UUID
	OwnerID         *uuid.UUID
	Status          *SnapshotStatus
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type UpdateSnapshotMetaParams struct {
	ID          uuid.UUID
	Name        *string
	Description *string
}
