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
	// VMRowModRevision is the etcd ModRevision of the vms row the heartbeat read
	// to decide this runtime claim. UpsertVMRuntime commits the runtime write
	// under If(ModRevision(vmKey)==VMRowModRevision), so a soft-delete or a
	// migration cutover (both write the vms row) landing between the read and the
	// commit fails the compare and skips the stale write. Zero means "no CAS"
	// (unconditional write) - etcd revisions are always >= 1, so the sentinel is
	// unambiguous; only the heartbeat caller sets it.
	VMRowModRevision int64
}
