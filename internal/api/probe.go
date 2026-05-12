// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// SystemLivenessProbeArgs is the JobArgs for a no-op job that proves
// the in-process queue is live end-to-end: the api binary inserted a
// job, river's fetcher picked it up, the worker pool dispatched it,
// and the worker returned. Operators trigger one with
// InsertLivenessProbe and observe the resulting INFO log to confirm
// the queue is alive without touching any domain table.
//
// The probe is not periodic by design — it is an ops surface, not a
// heartbeat. It also satisfies river's "at least one worker must be
// registered before Start" precondition when no domain workers
// (refresh-token cleanup, idempotency cleanup, …) have registered
// their own bundles yet, and remains in place as a useful operator
// tool.
type SystemLivenessProbeArgs struct{}

// Kind names the probe job kind. The `system.` domain prefix matches
// the binary-level scope the probe operates at (the api process
// itself, not any control-plane resource domain).
func (SystemLivenessProbeArgs) Kind() string { return "system.liveness_probe" }

// SystemLivenessProbeWorker logs an INFO record on Work, identifying
// the api process that handled the probe. Hostname and Version are
// captured once at registration; they reflect the running binary.
type SystemLivenessProbeWorker struct {
	river.WorkerDefaults[SystemLivenessProbeArgs]
	Hostname string
	Version  string
	Logger   *slog.Logger
}

// Work emits a single INFO record. The job carries no payload — every
// fact in the log line is a property of the binary running it.
func (w *SystemLivenessProbeWorker) Work(ctx context.Context, _ *river.Job[SystemLivenessProbeArgs]) error {
	w.Logger.InfoContext(ctx, "system.liveness_probe ran",
		"hostname", w.Hostname,
		"version", w.Version,
	)
	return nil
}

// InsertLivenessProbe enqueues a SystemLivenessProbeArgs job. The
// returned error reflects only the enqueue step; observing the
// resulting Work record requires a slog backend or the river
// subscription API.
func InsertLivenessProbe(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if _, err := client.Insert(ctx, SystemLivenessProbeArgs{}, nil); err != nil {
		return err
	}
	return nil
}
