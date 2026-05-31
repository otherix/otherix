// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CountStorageImagesBySHA256InPoolParams struct {
	PoolID         uuid.UUID
	ChecksumSha256 string
	ExcludeID      uuid.UUID
}

type CreateStorageImageParams struct {
	ID             uuid.UUID
	TemplateID     uuid.UUID
	PoolID         uuid.UUID
	ChecksumSha256 string
	SizeBytes      int64
	Format         string
}

type GetStorageImageByTemplateAndPoolParams struct {
	TemplateID uuid.UUID
	PoolID     uuid.UUID
}

type ListStorageImagesByPoolParams struct {
	PoolID           uuid.UUID
	CursorImportedAt *time.Time
	CursorID         *uuid.UUID
	LimitCount       int32
}
