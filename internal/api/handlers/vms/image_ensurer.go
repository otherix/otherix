// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"

	"github.com/otherix/otherix/internal/store"
)

// ImageEnsurer materializes a template's image on a storage pool before a
// vm-create is dispatched to that pool's node, so a create on a cold pool no
// longer hard-fails ErrTemplateMissing. Implementations are idempotent and a
// no-op when the image is already present (the IfNotPresent pull policy).
type ImageEnsurer interface {
	EnsureImageOnPool(ctx context.Context, template store.Template, pool store.StoragePool, node store.Node) error
}
