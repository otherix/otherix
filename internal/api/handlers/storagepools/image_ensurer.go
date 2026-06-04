// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/store"
)

// imageEnsureStore is the narrow store seam ImageEnsurer depends on
// (Go-style "consumer defines"). *etcdstore.Store satisfies it
// structurally; tests pass a fake without importing etcdstore.
type imageEnsureStore interface {
	StorageImageExists(ctx context.Context, templateID, poolID uuid.UUID) (bool, error)
	EnsureStorageImageProjected(ctx context.Context, img store.CreateStorageImageParams, tmplChecksum store.UpdateTemplateImageChecksumIfNullParams) error
}

// ImageEnsurer is the production implementation that materializes a
// template's image on a storage pool inline before a vm-create is
// dispatched to that pool's node. It is the IfNotPresent pull policy:
// a fast no-op when the image is already projected, otherwise a
// synchronous import drive against the owning agent.
//
// Unlike agentImportExecutor it does NOT persist the import's
// agent_task_id; a CP restart mid-import simply re-drives the
// idempotent, content-addressed import.
//
// *ImageEnsurer structurally satisfies vms.ImageEnsurer; the vms
// package wires it in without importing this one.
type ImageEnsurer struct {
	client agentImportClient
	st     imageEnsureStore
}

// NewImageEnsurer returns the production ImageEnsurer driving inline
// image materialization against an agent.
func NewImageEnsurer(client agentImportClient, st imageEnsureStore) *ImageEnsurer {
	return &ImageEnsurer{client: client, st: st}
}

// EnsureImageOnPool materializes template's image on pool, hosted by
// node, returning nil once the image is present (or already was). It
// is idempotent: when the image is already projected it returns
// without contacting the agent.
func (e *ImageEnsurer) EnsureImageOnPool(ctx context.Context, template store.Template, pool store.StoragePool, node store.Node) error {
	exists, err := e.st.StorageImageExists(ctx, template.ID, pool.ID)
	if err != nil {
		return fmt.Errorf("check storage image exists: %w", err)
	}
	if exists {
		return nil
	}

	idemKey := uuid.NewString()
	url := template.ImageUrl
	body := agentapi.ImageImportRequest{
		SourceURL: &url,
		Format:    agentapi.ImageImportRequestFormat(template.ImageFormat),
	}
	// nil bytes -> compute mode (agent computes the checksum during
	// streaming download). Non-nil bytes -> verify mode (agent compares
	// against this value, failing with checksum_mismatch on disagreement).
	if len(template.ImageChecksumSha256) > 0 {
		hexChecksum := hex.EncodeToString(template.ImageChecksumSha256)
		body.ExpectedChecksumSha256 = &hexChecksum
	}

	agentTaskID, err := e.client.PostImageImport(ctx, node.AdvertisedEndpoint, pool.Name, idemKey, body)
	if err != nil {
		return fmt.Errorf("post image import: %w", err)
	}

	terminal, err := e.client.PollTask(ctx, node.AdvertisedEndpoint, agentTaskID)
	if err != nil {
		return fmt.Errorf("poll image import: %w", err)
	}

	var result ImportResult
	switch terminal.Status {
	case "success":
		result, err = decodeImportResult(terminal.Result)
		if err != nil {
			return err
		}
	case "failed", "cancelled":
		if terminal.Error != nil {
			return terminal.Error
		}
		return fmt.Errorf("agent import task %s without error envelope", terminal.Status)
	default:
		return fmt.Errorf("unexpected agent terminal status %q", terminal.Status)
	}

	checksumBytes, err := hex.DecodeString(result.ChecksumSHA256)
	if err != nil {
		return fmt.Errorf("decode result checksum: %v", err)
	}
	if err := e.st.EnsureStorageImageProjected(ctx,
		store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     template.ID,
			PoolID:         pool.ID,
			ChecksumSha256: result.ChecksumSHA256,
			SizeBytes:      result.SizeBytes,
			Format:         result.Format,
		},
		store.UpdateTemplateImageChecksumIfNullParams{
			ID:                  template.ID,
			ImageChecksumSha256: checksumBytes,
		},
	); err != nil {
		return fmt.Errorf("project storage image: %w", err)
	}
	return nil
}
