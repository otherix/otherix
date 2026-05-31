// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type UpdateVMRuntimePhaseParams struct {
	Phase VMPhase
	VmID  uuid.UUID
}

type UpsertVMRuntimeParams struct {
	VmID               uuid.UUID
	CurrentNodeID      *uuid.UUID
	Phase              VMPhase
	ObservedGeneration int64
	QEMUPID            *int32
	LastStartedAt      *time.Time
	LastErrorMessage   *string
}
