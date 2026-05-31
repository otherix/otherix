// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedPool creates a ready node and a pool on it, returning the pool id.
func seedPool(t *testing.T, s *etcdstore.Store) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	np := nodeParams(uniqueNodeName("pool"))
	if _, err := s.CreateNode(ctx, np); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	pp := poolParams(np.ID, uniquePoolName("scan"))
	if _, err := s.CreateStoragePool(ctx, pp); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	return pp.ID
}

func TestProjectStoragePoolScan(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	poolID := seedPool(t, s)

	task := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, task, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}

	capacity := int64(500 * bytesPerGiBTest)
	available := int64(300 * bytesPerGiBTest)
	since := time.Now().UTC().Add(-time.Minute)
	if err := s.ProjectStoragePoolScan(ctx,
		store.UpsertStoragePoolUsageParams{ID: poolID, CapacityBytes: &capacity, AvailableBytes: &available},
		store.UpdatePoolDiskPressureParams{ID: poolID, DiskPressureSince: &since, DiskPressureCount: 3},
		store.UpdateTaskFinalizedParams{ID: task.ID, Status: store.TaskStatusSuccess, Result: []byte(`{"capacity_bytes":1}`)},
	); err != nil {
		t.Fatalf("ProjectStoragePoolScan: %v", err)
	}

	pool, err := s.StoragePoolByID(ctx, poolID)
	if err != nil {
		t.Fatalf("StoragePoolByID: %v", err)
	}
	if pool.CapacityBytes == nil || *pool.CapacityBytes != capacity {
		t.Errorf("capacity = %v, want %d", pool.CapacityBytes, capacity)
	}
	if pool.AvailableBytes == nil || *pool.AvailableBytes != available {
		t.Errorf("available = %v, want %d", pool.AvailableBytes, available)
	}
	if pool.ReportedAt == nil {
		t.Errorf("reported_at not stamped")
	}
	if pool.DiskPressureSince == nil || pool.DiskPressureCount != 3 {
		t.Errorf("pressure = (%v, %d), want (set, 3)", pool.DiskPressureSince, pool.DiskPressureCount)
	}
	got, _ := s.TaskByID(ctx, task.ID)
	if got.Status != store.TaskStatusSuccess || got.FinishedAt == nil {
		t.Errorf("task = %+v, want success + finished", got)
	}
}

func TestProjectStorageImageImport(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	_, poolID, templateID, _ := schedulingFixture(t, s)

	task := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, task, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	checksum := bytes.Repeat([]byte{0xAB}, 32)
	imageID := uuid.New()
	if err := s.ProjectStorageImageImport(ctx,
		store.CreateStorageImageParams{
			ID: imageID, TemplateID: templateID, PoolID: poolID,
			ChecksumSha256: "ab-checksum", SizeBytes: 1234, Format: "qcow2",
		},
		store.UpdateTemplateImageChecksumIfNullParams{ID: templateID, ImageChecksumSha256: checksum},
		store.UpdateTaskFinalizedParams{ID: task.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectStorageImageImport: %v", err)
	}

	imgs, err := s.ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{PoolID: poolID, LimitCount: 50})
	if err != nil || len(imgs) != 1 || imgs[0].ChecksumSha256 != "ab-checksum" || imgs[0].ID != imageID {
		t.Fatalf("images = (%v, %v), want one %q with id %v", imgs, err, "ab-checksum", imageID)
	}
	tpl, _ := s.TemplateByID(ctx, templateID)
	if !bytes.Equal(tpl.ImageChecksumSha256, checksum) {
		t.Errorf("template checksum back-prop = %x, want %x", tpl.ImageChecksumSha256, checksum)
	}
	got, _ := s.TaskByID(ctx, task.ID)
	if got.Status != store.TaskStatusSuccess {
		t.Errorf("task = %+v, want success", got)
	}

	// Re-import to the same (template, pool): upsert keeps the id, refreshes
	// checksum/size, and does NOT overwrite the now-set template checksum.
	task2 := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, task2, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(2): %v", err)
	}
	if err := s.ProjectStorageImageImport(ctx,
		store.CreateStorageImageParams{
			ID: uuid.New(), TemplateID: templateID, PoolID: poolID,
			ChecksumSha256: "cd-checksum", SizeBytes: 5678, Format: "qcow2",
		},
		store.UpdateTemplateImageChecksumIfNullParams{ID: templateID, ImageChecksumSha256: bytes.Repeat([]byte{0xCD}, 32)},
		store.UpdateTaskFinalizedParams{ID: task2.ID, Status: store.TaskStatusSuccess},
	); err != nil {
		t.Fatalf("ProjectStorageImageImport(reimport): %v", err)
	}
	imgs, _ = s.ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{PoolID: poolID, LimitCount: 50})
	if len(imgs) != 1 || imgs[0].ID != imageID || imgs[0].ChecksumSha256 != "cd-checksum" || imgs[0].SizeBytes != 5678 {
		t.Errorf("after reimport images = %+v, want single id %v with cd-checksum/5678", imgs, imageID)
	}
	tpl, _ = s.TemplateByID(ctx, templateID)
	if !bytes.Equal(tpl.ImageChecksumSha256, checksum) {
		t.Errorf("template checksum after reimport = %x, want unchanged %x", tpl.ImageChecksumSha256, checksum)
	}
}
