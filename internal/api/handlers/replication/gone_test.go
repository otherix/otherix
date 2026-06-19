// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

type goneBlobStoreSpy struct{ pruned []uuid.UUID }

func (s *goneBlobStoreSpy) UpsertNodeBlobInventory(_ context.Context, id uuid.UUID, blobs []store.NodeBlob) error {
	if len(blobs) == 0 {
		s.pruned = append(s.pruned, id)
	}
	return nil
}

func TestPruneGoneNodeBlobsClearsEachGoneNode(t *testing.T) {
	st := &goneBlobStoreSpy{}
	a, b := uuid.New(), uuid.New()
	hook := PruneGoneNodeBlobsFunc(st, discardLog())
	if err := hook(context.Background(), []store.MarkNodesGoneRow{{ID: a, Name: "node-1"}, {ID: b, Name: "node-2"}}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(st.pruned) != 2 || st.pruned[0] != a || st.pruned[1] != b {
		t.Errorf("pruned = %v, want [%s %s]", st.pruned, a, b)
	}
}
