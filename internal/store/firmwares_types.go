// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateFirmwareParams struct {
	ID           uuid.UUID
	Name         string
	Architecture CPUArch
	Type         FirmwareType
	Version      *string
	SecureBoot   bool
	IsDefault    bool
}

type GetDefaultFirmwareForArchAndTypeParams struct {
	Architecture CPUArch
	Type         FirmwareType
}

type ListFirmwaresParams struct {
	Architecture    *CPUArch
	Type            *FirmwareType
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type LookupFirmwareByCatalogParams struct {
	Name         string
	Architecture CPUArch
	Type         FirmwareType
}

type UpdateFirmwareParams struct {
	Name       string
	Version    *string
	SecureBoot bool
	IsDefault  bool
	ID         uuid.UUID
}
