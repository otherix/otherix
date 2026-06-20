// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// nodeBlobsSpy records the per-node artifact-tier inventory writes
// applyBlobInventory makes. It embeds store.HeartbeatProjection (left nil) so it
// satisfies the interface while implementing only the method applyBlobInventory
// exercises; any other call would panic, the desired failure mode for an
// unexpected projection step.
type nodeBlobsSpy struct {
	store.HeartbeatProjection
	blobs        map[uuid.UUID][]store.NodeBlob
	upsertCalled bool
}

func (s *nodeBlobsSpy) UpsertNodeBlobInventory(_ context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error {
	s.upsertCalled = true
	if s.blobs == nil {
		s.blobs = make(map[uuid.UUID][]store.NodeBlob)
	}
	s.blobs[nodeID] = blobs
	return nil
}

// TestApplyBlobInventoryClampsSize verifies the artifact-tier size clamp: a
// negative or over-ceiling SizeBytes is clamped to 0 (still counted as a holder,
// but no node-supplied pull bound) while a normal size passes through. The
// reported size becomes the consumer's pull cap, so an unvalidated value would
// raise the cap arbitrarily (huge) or disable it (negative, the consumer's > 0
// gate).
func TestApplyBlobInventoryClampsSize(t *testing.T) {
	nodeID := uuid.New()
	normal := strings.Repeat("a", 64)
	negative := strings.Repeat("b", 64)
	huge := strings.Repeat("c", 64)

	body := &requestBody{
		Blobs: []blobReport{
			{Digest: normal, SizeBytes: 4096},
			{Digest: negative, SizeBytes: -1},
			{Digest: huge, SizeBytes: maxBlobSizeBytes + 1},
		},
	}

	spy := &nodeBlobsSpy{}
	h := newQuietHandler()
	if err := h.applyBlobInventory(context.Background(), spy, nodeID, body); err != nil {
		t.Fatalf("applyBlobInventory returned error: %v", err)
	}
	if !spy.upsertCalled {
		t.Fatalf("UpsertNodeBlobInventory not called")
	}
	want := []store.NodeBlob{
		{Digest: normal, SizeBytes: 4096},
		{Digest: negative, SizeBytes: 0},
		{Digest: huge, SizeBytes: 0},
	}
	if diff := cmp.Diff(want, spy.blobs[nodeID]); diff != "" {
		t.Errorf("node blob inventory after clamp mismatch (-want +got):\n%s", diff)
	}
}
