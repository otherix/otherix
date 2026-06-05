// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "github.com/google/uuid"

type CreateVMDiskParams struct {
	VmID          uuid.UUID
	StoragePoolID uuid.UUID
	DeviceOrder   int32
	Bus           DiskBus
	SizeGib       int32
	SourceKind    string
	Format        ImageFormat
	ReadOnly      bool
	CacheMode     DiskCacheMode
	Discard       DiskDiscard
	BootOrder     *int32
}
