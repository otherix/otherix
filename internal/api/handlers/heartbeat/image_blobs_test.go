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

// imageBlobsSpy records the per-node image-blob inventory writes
// applyImageBlobInventory makes. It embeds store.HeartbeatProjection (left nil)
// so it satisfies the interface while implementing only the method
// applyImageBlobInventory exercises; any other call would panic, the desired
// failure mode for an unexpected projection step.
type imageBlobsSpy struct {
	store.HeartbeatProjection
	// imageBlobs records the inventory passed to UpsertNodeImageBlobInventory.
	imageBlobs map[uuid.UUID][]store.NodeBlob
	// imageUpsertCalled is true once UpsertNodeImageBlobInventory ran.
	imageUpsertCalled bool
}

func (s *imageBlobsSpy) UpsertNodeImageBlobInventory(_ context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error {
	s.imageUpsertCalled = true
	if s.imageBlobs == nil {
		s.imageBlobs = make(map[uuid.UUID][]store.NodeBlob)
	}
	s.imageBlobs[nodeID] = blobs
	return nil
}

// TestApplyImageBlobInventoryValidatesAndUpserts verifies the consumer side of
// the image-cache inventory seam: a valid 64-char hex digest is kept and a
// malformed one is dropped, and UpsertNodeImageBlobInventory receives exactly
// the good entry.
func TestApplyImageBlobInventoryValidatesAndUpserts(t *testing.T) {
	nodeID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	good := strings.Repeat("a", 64)

	body := &requestBody{
		ImageBlobs: []blobReport{
			{Digest: good, SizeBytes: 100},
			{Digest: "not-hex", SizeBytes: 200},
		},
	}

	spy := &imageBlobsSpy{}
	h := newQuietHandler()
	if err := h.applyImageBlobInventory(context.Background(), spy, nodeID, body); err != nil {
		t.Fatalf("applyImageBlobInventory returned error: %v", err)
	}
	if !spy.imageUpsertCalled {
		t.Fatalf("UpsertNodeImageBlobInventory not called")
	}
	want := []store.NodeBlob{{Digest: good, SizeBytes: 100}}
	if diff := cmp.Diff(want, spy.imageBlobs[nodeID]); diff != "" {
		t.Errorf("image blob inventory mismatch (-want +got):\n%s", diff)
	}
}

// TestApplyImageBlobInventoryUnavailablePreserves verifies that on
// image_blobs_unavailable the upsert is skipped entirely so a transient
// agent enumerate-failure preserves the prior inventory rather than clearing it.
func TestApplyImageBlobInventoryUnavailablePreserves(t *testing.T) {
	nodeID := uuid.New()
	body := &requestBody{
		ImageBlobsUnavailable: true,
		ImageBlobs:            []blobReport{{Digest: strings.Repeat("a", 64), SizeBytes: 1}},
	}

	spy := &imageBlobsSpy{}
	h := newQuietHandler()
	if err := h.applyImageBlobInventory(context.Background(), spy, nodeID, body); err != nil {
		t.Fatalf("applyImageBlobInventory returned error: %v", err)
	}
	if spy.imageUpsertCalled {
		t.Errorf("UpsertNodeImageBlobInventory called, want skipped on image_blobs_unavailable")
	}
}
