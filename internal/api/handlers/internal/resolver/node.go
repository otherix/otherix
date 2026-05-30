// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package resolver

import (
	"context"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Node looks up a node by name. UUID literals are rejected with
// CodeUUIDInName before any DB roundtrip. Internal callers that
// already hold a UUID can address store.Queries().GetNodeByID
// directly.
//
// Name matching is case-insensitive (uq_nodes_name on lower(name)).
func Node(ctx context.Context, q Querier, identifier string) (store.Node, error) {
	if _, err := uuid.Parse(identifier); err == nil {
		return store.Node{}, &Error{Kind: KindNode, Identifier: identifier, Code: CodeUUIDInName}
	}
	row, err := q.NodeByName(ctx, identifier)
	if err != nil {
		return store.Node{}, wrapLookupErr(KindNode, identifier, err)
	}
	return row, nil
}
