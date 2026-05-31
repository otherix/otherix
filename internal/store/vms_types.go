// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateVMParams struct {
	ID                uuid.UUID
	OwnerID           uuid.UUID
	Name              string
	Description       string
	TemplateID        *uuid.UUID
	Architecture      CPUArch
	CpuCores          int32
	MemoryMib         int32
	CPUModel          string
	MachineType       string
	FirmwareID        *uuid.UUID
	PinnedNodeID      *uuid.UUID
	UserData          *string
	CloudInitDisabled bool
	Labels            []byte
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
