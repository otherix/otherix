// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/otherix/otherix/internal/api/agentclient"
	heartbeathandlers "github.com/otherix/otherix/internal/api/handlers/heartbeat"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	taskshandlers "github.com/otherix/otherix/internal/api/handlers/tasks"
	vmshandlers "github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
	"github.com/otherix/otherix/internal/version"
)

// RiverDeps bundles the dependencies BuildRiverClient consumes.
//
// ScanExecutor / ImportExecutor / AgentClient are optional: when
// non-nil they wire the corresponding production worker; when nil
// the worker is skipped. This lets tests construct a river client
// purely for tasks.cancel's JobCancelTx without spinning up agent
// machinery, and lets `cmd/api/main.go::startRiver` skip agent
// wiring cleanly when `Workers.Enabled=false`.
type RiverDeps struct {
	Pool                *pgxpool.Pool
	Cfg                 config.WorkersConfig
	Logger              *slog.Logger
	Store               *store.Store
	ScanExecutor        storagepoolshandlers.ScanExecutor
	ImportExecutor      storagepoolshandlers.ImportExecutor
	VMCreateExecutor    vmshandlers.CreateExecutor
	VMDeleteExecutor    vmshandlers.DeleteExecutor
	VMLifecycleExecutor vmshandlers.LifecycleExecutor
	AgentClient         *agentclient.Client
	PressureDisk        config.PressureConditionConfig
}

// validate enforces the construction-time preconditions of
// BuildRiverClient. Logger is intentionally not nil-checked — every
// callsite (production + tests) passes a non-nil logger today.
func (d RiverDeps) validate() error {
	if d.Pool == nil {
		return fmt.Errorf("river client: pool is nil")
	}
	if d.Store == nil {
		return fmt.Errorf("river client: store is nil")
	}
	if d.Cfg.MaxWorkers <= 0 {
		return fmt.Errorf("river client: workers.max_workers must be > 0 (got %d)", d.Cfg.MaxWorkers)
	}
	return nil
}

// BuildRiverClient constructs the in-process *river.Client wired
// against the supplied pgx pool and store.
//
// Workers shipped from this bundle:
//
//   - a SystemLivenessProbeWorker (operator-facing liveness probe; also
//     satisfies river's at-least-one-worker precondition for Start);
//   - the auth-domain workers and periodic schedules (refresh-token
//     cleanup);
//   - the idempotency-keys cleanup worker;
//   - the storage-pools scan worker (executor seam) — only when
//     deps.ScanExecutor is non-nil;
//   - the storage-pools scan-trigger periodic worker — fans out per-
//     pool scan tasks on a fixed cadence; periodic schedule registered
//     only when deps.Cfg.StoragePoolScan.Enabled is true;
//   - the storage-image import worker — only when deps.ImportExecutor
//     is non-nil;
//   - the tasks cleanup periodic worker.
func BuildRiverClient(deps RiverDeps) (*river.Client[pgx.Tx], error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &SystemLivenessProbeWorker{
		Hostname: hostname,
		Version:  version.Current().Version,
		Logger:   deps.Logger,
	})
	auth.Workers(workers, auth.Deps{Store: deps.Store, Logger: deps.Logger})
	middleware.IdempotencyWorkers(workers, middleware.IdempotencyDeps{Store: deps.Store, Logger: deps.Logger})
	scanDeps := storagepoolshandlers.ScanDeps{
		Store:        deps.Store,
		Executor:     deps.ScanExecutor,
		Logger:       deps.Logger,
		PressureDisk: deps.PressureDisk,
	}
	// Avoid the typed-nil-in-interface trap: only assign Lister when
	// the pointer is non-nil so the worker's `lister == nil` check
	// behaves as expected at the call site.
	if deps.AgentClient != nil {
		scanDeps.Lister = deps.AgentClient
	}
	storagepoolshandlers.ScanWorkers(workers, scanDeps)
	scanTriggerDeps := storagepoolshandlers.ScanTriggerDeps{
		Store: deps.Store,
		Config: storagepoolshandlers.ScanTriggerConfig{
			Interval: deps.Cfg.StoragePoolScan.Interval,
			Jitter:   deps.Cfg.StoragePoolScan.Jitter,
		},
		Logger: deps.Logger,
	}
	storagepoolshandlers.ScanTriggerWorkers(workers, scanTriggerDeps)
	storagepoolshandlers.ImportWorkers(workers, storagepoolshandlers.ImportDeps{
		Store:    deps.Store,
		Executor: deps.ImportExecutor,
		Logger:   deps.Logger,
	})
	vmshandlers.Workers(workers,
		vmshandlers.CreateDeps{
			Store:    deps.Store,
			Executor: deps.VMCreateExecutor,
			Logger:   deps.Logger,
		},
		vmshandlers.DeleteDeps{
			Store:    deps.Store,
			Executor: deps.VMDeleteExecutor,
			Logger:   deps.Logger,
		},
	)
	vmshandlers.LifecycleWorkers(workers, vmshandlers.LifecycleDepsAsync{
		Store:    deps.Store,
		Executor: deps.VMLifecycleExecutor,
		Logger:   deps.Logger,
	})
	tasksDeps := taskshandlers.Deps{
		Store: deps.Store,
		Retention: taskshandlers.RetentionConfig{
			Completed: deps.Cfg.Tasks.Retention.Completed,
			Failed:    deps.Cfg.Tasks.Retention.Failed,
		},
		Logger: deps.Logger,
	}
	taskshandlers.Workers(workers, tasksDeps)

	heartbeatDeps := heartbeathandlers.ReconcileDeps{
		Store: deps.Store,
		Config: heartbeathandlers.ReconcileConfig{
			StaleThreshold: deps.Cfg.Heartbeat.StaleThreshold,
			Interval:       deps.Cfg.Heartbeat.Interval,
		},
		Logger: deps.Logger,
	}
	heartbeathandlers.Workers(workers, heartbeatDeps)

	periodic := auth.PeriodicJobs(auth.Deps{Store: deps.Store, Logger: deps.Logger})
	periodic = append(periodic, middleware.IdempotencyPeriodicJobs(middleware.IdempotencyDeps{Store: deps.Store, Logger: deps.Logger})...)
	periodic = append(periodic, taskshandlers.PeriodicJobs(tasksDeps)...)
	periodic = append(periodic, heartbeathandlers.PeriodicJobs(heartbeatDeps)...)
	if deps.Cfg.StoragePoolScan.Enabled {
		periodic = append(periodic, storagepoolshandlers.ScanTriggerPeriodicJobs(scanTriggerDeps)...)
	}

	c, err := river.NewClient(riverpgxv5.New(deps.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: deps.Cfg.MaxWorkers},
		},
		Workers:      workers,
		PeriodicJobs: periodic,
		Logger:       deps.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("river new client: %v", err)
	}
	return c, nil
}
