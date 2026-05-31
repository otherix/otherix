// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type ListNodeFirmwaresParams struct {
	NodeID          uuid.UUID
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListNodeFirmwaresRow struct {
	NodeFirmware NodeFirmware
	Firmware     Firmware
}

type UpsertNodeFirmwareParams struct {
	NodeID     uuid.UUID
	FirmwareID uuid.UUID
	CodePath   string
	VarsPath   *string
	Available  bool
}
