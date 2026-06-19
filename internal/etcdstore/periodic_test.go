// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the periodic workers' storage contracts.
var (
	_ taskshandlers.CleanupStore            = (*etcdstore.Store)(nil)
	_ heartbeathandlers.ReconcileStore      = (*etcdstore.Store)(nil)
	_ storagepoolshandlers.ScanTriggerStore = (*etcdstore.Store)(nil)
)

func TestScanTriggerFuncEnqueues(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_ = seedPool(t, s) // ready-ish (pending) node + pool => needs scan

	fn := storagepoolshandlers.ScanTriggerFunc(s, log)
	if err := fn(ctx); err != nil {
		t.Fatalf("ScanTriggerFunc: %v", err)
	}

	tasks, _ := s.ListTasksAny(ctx, store.ListTasksAnyParams{LimitCount: 50})
	var scanTasks int
	for _, tk := range tasks {
		if tk.Type == "storage_pool.scan" {
			scanTasks++
		}
	}
	if scanTasks != 1 {
		t.Errorf("scan tasks enqueued = %d, want 1", scanTasks)
	}
	jobs, _ := s.PendingJobs(ctx)
	if len(jobs) != 1 {
		t.Errorf("pending jobs = %d, want 1", len(jobs))
	}

	// Second tick is a no-op: the in-flight scan task suppresses re-enqueue.
	if err := fn(ctx); err != nil {
		t.Fatalf("ScanTriggerFunc#2: %v", err)
	}
	tasks, _ = s.ListTasksAny(ctx, store.ListTasksAnyParams{LimitCount: 50})
	scanTasks = 0
	for _, tk := range tasks {
		if tk.Type == "storage_pool.scan" {
			scanTasks++
		}
	}
	if scanTasks != 1 {
		t.Errorf("scan tasks after second tick = %d, want still 1", scanTasks)
	}
}

func TestReconcileFuncPromotes(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	np := nodeParams(uniqueNodeName("reconcile"))
	if _, err := s.CreateNode(ctx, np); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	bumpHeartbeat(t, s, np.ID)

	fn := heartbeathandlers.ReconcileFunc(s, heartbeathandlers.ReconcileConfig{}, log, nil, nil)
	if err := fn(ctx); err != nil {
		t.Fatalf("ReconcileFunc: %v", err)
	}
	n, _ := s.NodeByID(ctx, np.ID)
	if n.Status != store.NodeStatusReady {
		t.Errorf("node status = %v, want ready", n.Status)
	}
}

func TestCleanupFuncRunsClean(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fn := taskshandlers.CleanupFunc(s, taskshandlers.RetentionConfig{}, log)
	if err := fn(ctx); err != nil {
		t.Errorf("CleanupFunc: %v", err)
	}
}
