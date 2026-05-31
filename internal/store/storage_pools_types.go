// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateStoragePoolParams struct {
	ID     uuid.UUID
	NodeID uuid.UUID
	Name   string
	Type   string
	Path   string
	Config []byte
}

type GetPoolDiskPressureStateRow struct {
	DiskPressureSince *time.Time
	DiskPressureCount int32
}

type ListDiskPressuredPoolsByNameRow struct {
	PoolEffectiveCapacity     PoolEffectiveCapacity
	NodeEffectiveAvailability NodeEffectiveAvailability
}

type ListEligiblePoolsByNameRow struct {
	PoolEffectiveCapacity     PoolEffectiveCapacity
	NodeEffectiveAvailability NodeEffectiveAvailability
}

type ListMemoryPressuredCandidatesByNameRow struct {
	PoolEffectiveCapacity     PoolEffectiveCapacity
	NodeEffectiveAvailability NodeEffectiveAvailability
}

type ListPoolsEffectiveParams struct {
	NodeID          *uuid.UUID
	Type            *string
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListPoolsNeedingScanRow struct {
	ID     uuid.UUID
	Name   string
	NodeID uuid.UUID
}

type ListStoragePoolsParams struct {
	NodeID          *uuid.UUID
	Type            *string
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListSystemDiskPressuredCandidatesByNameRow struct {
	PoolEffectiveCapacity     PoolEffectiveCapacity
	NodeEffectiveAvailability NodeEffectiveAvailability
}

type UpdatePoolDiskPressureParams struct {
	DiskPressureSince *time.Time
	DiskPressureCount int32
	ID                uuid.UUID
}

type UpdateStoragePoolParams struct {
	Name   string
	Config []byte
	ID     uuid.UUID
}

type UpdateStoragePoolReconciliationParams struct {
	ReconciliationStatus string
	ReconciliationError  *string
	NodeID               uuid.UUID
	Name                 string
}

type UpsertStoragePoolUsageParams struct {
	CapacityBytes  *int64
	AvailableBytes *int64
	ID             uuid.UUID
}
