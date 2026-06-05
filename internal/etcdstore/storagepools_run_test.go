// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the storage-pool worker handlers' storage contract.
var _ storagepoolshandlers.WorkerStore = (*etcdstore.Store)(nil)

type stubScanExec struct {
	res storagepoolshandlers.ScanResult
	err error
}

func (s stubScanExec) Execute(context.Context, storagepoolshandlers.ScanArgs) (storagepoolshandlers.ScanResult, error) {
	return s.res, s.err
}

func TestStoragePoolScanRunHandler(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	poolID := seedPool(t, s)

	task := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, task, testJobArgs{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	raw, _ := json.Marshal(storagepoolshandlers.StoragePoolScanArgs{TaskID: task.ID, PoolID: poolID})
	exec := stubScanExec{res: storagepoolshandlers.ScanResult{CapacityBytes: 1000, AvailableBytes: 700}}
	h := storagepoolshandlers.ScanHandler(s, exec, config.PressureConditionConfig{}, log)
	if err := h(ctx, raw); err != nil {
		t.Fatalf("scan handler: %v", err)
	}

	pool, _ := s.StoragePoolByID(ctx, poolID)
	if pool.CapacityBytes == nil || *pool.CapacityBytes != 1000 || pool.AvailableBytes == nil || *pool.AvailableBytes != 700 {
		t.Errorf("pool usage = (%v, %v), want 1000/700", pool.CapacityBytes, pool.AvailableBytes)
	}
	got, _ := s.TaskByID(ctx, task.ID)
	if got.Status != store.TaskStatusSuccess {
		t.Errorf("task = %v, want success", got.Status)
	}
}
