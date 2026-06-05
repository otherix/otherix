// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
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
