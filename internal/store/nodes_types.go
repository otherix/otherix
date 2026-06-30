// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CancelActiveMigrationsOnNodeParams struct {
	Reason string
	NodeID uuid.UUID
}

type CreateNodeParams struct {
	ID   uuid.UUID
	Name string
	// Kind is the node role; empty defaults to NodeKindNode at create time. Not
	// accepted from the admin create-node request - gateways self-register on join.
	Kind                    string
	Architecture            CPUArch
	AdvertisedEndpoint      string
	MigrationHost           string
	MigrationPortRangeStart int32
	MigrationPortRangeEnd   int32
	Status                  NodeStatus
}

type GetNodeForHeartbeatRow struct {
	ID                      uuid.UUID
	Architecture            CPUArch
	Status                  NodeStatus
	MemoryPressureSince     *time.Time
	MemoryPressureCount     int32
	SystemDiskPressureSince *time.Time
	SystemDiskPressureCount int32
}

type ListNodesParams struct {
	Architecture    *CPUArch
	Status          *NodeStatus
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListNodesEffectiveParams struct {
	Architecture    *CPUArch
	Status          *NodeStatus
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type MarkNodesUnreachableRow struct {
	ID              uuid.UUID
	Name            string
	LastHeartbeatAt *time.Time
}

type MarkNodesGoneRow struct {
	ID   uuid.UUID
	Name string
}

type PromoteHealthyNodesRow struct {
	ID     uuid.UUID
	Name   string
	Status NodeStatus
}

type UpdateNodeHeartbeatParams struct {
	AgentVersion             *string
	MigrationHost            string
	MigrationPortRangeStart  int32
	MigrationPortRangeEnd    int32
	CPUCoresTotal            *int32
	CPUCoresAvailable        *int32
	CPUModel                 *string
	CpuFlags                 []string
	MemoryTotalMib           *int64
	MemoryAvailableMib       *int64
	Hugepages2mibTotal       *int32
	Hugepages1gibTotal       *int32
	KernelVersion            *string
	QEMUVersion              *string
	NumaTopology             []byte
	Capabilities             []byte
	SystemDiskTotalBytes     *int64
	SystemDiskAvailableBytes *int64
	ID                       uuid.UUID
}

type UpdateNodeMemoryPressureParams struct {
	MemoryPressureSince *time.Time
	MemoryPressureCount int32
	ID                  uuid.UUID
}

type UpdateNodeStatusParams struct {
	Status     NodeStatus
	CordonedAt *time.Time
	ID         uuid.UUID
}

type UpdateNodeSystemDiskPressureParams struct {
	SystemDiskPressureSince *time.Time
	SystemDiskPressureCount int32
	ID                      uuid.UUID
}
