// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package resolver

import (
	"context"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Template looks up a template by name. UUID literals are rejected
// with CodeUUIDInName before any DB roundtrip; operator-facing paths
// and body fields must speak names. Internal callers that already
// hold a UUID can address store.Queries().GetTemplate directly.
//
// Name matching is case-insensitive (see uq_templates_name on
// lower(name)); the returned row preserves the operator's original
// casing.
//
// Visibility / ownership are NOT enforced here — the resolver only
// answers "does a row with this name exist?". Handlers that need
// "and is this row visible to the caller?" run their own RBAC check
// (checkTemplateUseAccess, checkImportAccess, …) after the resolve.
func Template(ctx context.Context, q Querier, identifier string) (store.Template, error) {
	if _, err := uuid.Parse(identifier); err == nil {
		return store.Template{}, &Error{Kind: KindTemplate, Identifier: identifier, Code: CodeUUIDInName}
	}
	row, err := q.GetTemplateByName(ctx, identifier)
	if err != nil {
		return store.Template{}, wrapLookupErr(KindTemplate, identifier, err)
	}
	return row, nil
}
