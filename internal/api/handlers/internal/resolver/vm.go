// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package resolver

import (
	"context"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// VM looks up a VM by name. UUID literals are rejected with
// CodeUUIDInName before any DB roundtrip. Internal callers that
// already hold a UUID can address store.Queries().GetVMByID directly.
//
// Name matching is case-insensitive (uq_vms_name on lower(name)).
//
// Soft-deleted VMs are invisible here — GetVMByName filters on
// deleted_at IS NULL. A delete-then-recreate flow can reuse the name
// immediately.
func VM(ctx context.Context, q Querier, identifier string) (store.VM, error) {
	if _, err := uuid.Parse(identifier); err == nil {
		return store.VM{}, &Error{Kind: KindVM, Identifier: identifier, Code: CodeUUIDInName}
	}
	row, err := q.GetVMByName(ctx, identifier)
	if err != nil {
		return store.VM{}, wrapLookupErr(KindVM, identifier, err)
	}
	return row, nil
}
