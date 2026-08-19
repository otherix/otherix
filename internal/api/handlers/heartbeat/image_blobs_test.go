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

// TestApplyImageBlobInventoryClampsSize verifies the image-tier size clamp: a
// negative or over-ceiling SizeBytes is clamped to 0 (still a holder, but no
// node-supplied pull bound) while a normal size passes through. The reported
// size becomes the consumer's pull cap, so an unvalidated value would defeat or
// disable that cap.
func TestApplyImageBlobInventoryClampsSize(t *testing.T) {
	nodeID := uuid.New()
	normal := strings.Repeat("a", 64)
	negative := strings.Repeat("b", 64)
	huge := strings.Repeat("c", 64)

	body := &requestBody{
		ImageBlobs: []blobReport{
			{Digest: normal, SizeBytes: 4096},
			{Digest: negative, SizeBytes: -1},
			{Digest: huge, SizeBytes: maxBlobSizeBytes + 1},
		},
	}

	spy := &imageBlobsSpy{}
	h := newQuietHandler()
	if err := h.applyImageBlobInventory(context.Background(), spy, nodeID, body); err != nil {
		t.Fatalf("applyImageBlobInventory returned error: %v", err)
	}
	want := []store.NodeBlob{
		{Digest: normal, SizeBytes: 4096},
		{Digest: negative, SizeBytes: 0},
		{Digest: huge, SizeBytes: 0},
	}
	if diff := cmp.Diff(want, spy.imageBlobs[nodeID]); diff != "" {
		t.Errorf("image blob inventory after clamp mismatch (-want +got):\n%s", diff)
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

// VMSoftDeleted answers "not deleted" for every id: these tests do not drive
// the heartbeat teardown path. A test that does asserts on its own answer.
func (s *imageBlobsSpy) VMSoftDeleted(context.Context, uuid.UUID) (bool, string, error) {
	return false, "", nil
}
