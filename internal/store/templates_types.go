// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CloneTemplateParams struct {
	NewID          uuid.UUID
	NewOwnerID     uuid.UUID
	NewName        string
	NewDescription *string
	SourceID       uuid.UUID
}

type CreateTemplateParams struct {
	ID                     uuid.UUID
	OwnerID                uuid.UUID
	Name                   string
	Description            string
	Architecture           CPUArch
	OsFamily               OsFamily
	OsVariant              string
	ImageUrl               string
	ImageChecksumSha256    []byte
	ImageFormat            ImageFormat
	ImageSizeBytes         *int64
	FirmwareType           FirmwareType
	FirmwareID             *uuid.UUID
	DefaultCpuCores        int32
	DefaultMemoryMib       int32
	DefaultDiskGib         int32
	CloudInitSupported     bool
	CloudInitUserData      *string
	QemuGuestAgentExpected bool
	Metadata               []byte
}

type ListTemplatesParams struct {
	CallerCanSeeAny    bool
	CallerID           *uuid.UUID
	OwnerIDFilter      *uuid.UUID
	VisibilityFilter   *string
	ArchitectureFilter *CPUArch
	OsFamilyFilter     *OsFamily
	CursorCreatedAt    *time.Time
	CursorID           *uuid.UUID
	LimitCount         int32
}

type SetTemplateVisibilityParams struct {
	Visibility string
	ID         uuid.UUID
}

type UpdateTemplateParams struct {
	Name                   string
	Description            string
	OsVariant              string
	FirmwareType           FirmwareType
	FirmwareID             *uuid.UUID
	DefaultCpuCores        int32
	DefaultMemoryMib       int32
	DefaultDiskGib         int32
	CloudInitSupported     bool
	CloudInitUserData      *string
	QemuGuestAgentExpected bool
	Metadata               []byte
	ID                     uuid.UUID
}

type UpdateTemplateImageChecksumIfNullParams struct {
	ImageChecksumSha256 []byte
	ID                  uuid.UUID
}
