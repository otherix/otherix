// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
)

// fakeImportClient is the agent-facing seam the production
// storagepools.ImageEnsurer drives. It returns a canned successful import
// terminal (a Result carrying checksum_sha256 / size_bytes / format so
// decodeImportResult succeeds) and counts PostImageImport calls so the
// redelivery-skip invariant can be asserted: the second EnsureImageOnPool must
// NOT contact the agent because the projection from the first call makes
// StorageImageExists return true.
type fakeImportClient struct {
	postCalls atomic.Int32
}

func (c *fakeImportClient) PostImageImport(
	context.Context, string, string, string, agentapi.ImageImportRequest,
) (uuid.UUID, error) {
	c.postCalls.Add(1)
	return uuid.New(), nil
}

func (c *fakeImportClient) PollTask(
	context.Context, string, uuid.UUID,
) (agentclient.TaskTerminal, error) {
	return agentclient.TaskTerminal{
		Status: "success",
		Result: map[string]any{
			"checksum_sha256": "abcdef0123456789",
			"size_bytes":      float64(1 << 30),
			"format":          "qcow2",
		},
	}, nil
}

// TestImageEnsurerSeamRealStoreRedeliverySkip drives the REAL production
// storagepools.ImageEnsurer against a REAL *etcdstore.Store (the concrete
// imageEnsureStore). It proves the content-addressed redelivery-skip invariant
// through the real store projection, not a faked store:
//
//   - first EnsureImageOnPool contacts the agent once and the projection lands
//     (StorageImageExists flips false -> true);
//   - second EnsureImageOnPool (redelivery) skips the agent entirely because
//     StorageImageExists now resolves true, and returns nil.
//
// This is the "test the seam, not the unit" coverage for the B1 wiring: the
// vm.create worker reaching this ensurer on a cold pool materializes the image
// inline, and a redelivered create no longer re-imports.
func TestImageEnsurerSeamRealStoreRedeliverySkip(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	nodeID, poolID, templateID, _ := schedulingFixture(t, s)
	tpl, err := s.TemplateByID(ctx, templateID)
	if err != nil {
		t.Fatalf("TemplateByID: %v", err)
	}
	pool, err := s.StoragePoolByID(ctx, poolID)
	if err != nil {
		t.Fatalf("StoragePoolByID: %v", err)
	}
	node, err := s.NodeByID(ctx, nodeID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}

	client := &fakeImportClient{}
	ens := storagepoolshandlers.NewImageEnsurer(client, s)

	// Pre-state: no image projected yet.
	if exists, err := s.StorageImageExists(ctx, templateID, poolID); err != nil || exists {
		t.Fatalf("StorageImageExists(pre) = (%v, %v), want (false, nil)", exists, err)
	}

	// First ensure: drives the agent import once, projection lands.
	if err := ens.EnsureImageOnPool(ctx, tpl, pool, node); err != nil {
		t.Fatalf("EnsureImageOnPool(first): %v", err)
	}
	if got := client.postCalls.Load(); got != 1 {
		t.Fatalf("PostImageImport calls after first ensure = %d, want 1", got)
	}
	exists, err := s.StorageImageExists(ctx, templateID, poolID)
	if err != nil {
		t.Fatalf("StorageImageExists(post-first): %v", err)
	}
	if !exists {
		t.Fatalf("StorageImageExists(post-first) = false, want true (projection must land through the real store)")
	}

	// Second ensure (redelivery): the content-addressed guard short-circuits;
	// the agent is not contacted again.
	if err := ens.EnsureImageOnPool(ctx, tpl, pool, node); err != nil {
		t.Fatalf("EnsureImageOnPool(second): %v", err)
	}
	if got := client.postCalls.Load(); got != 1 {
		t.Errorf("PostImageImport calls after redelivery = %d, want 1 (redelivery must skip the agent)", got)
	}
}
