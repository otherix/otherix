// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/migrationtest"
	"github.com/otherix/otherix/internal/store"
)

// TestBuildRiverClientLifecycle exercises the river wiring end to
// end: build a client against an isolated Postgres testcontainer,
// subscribe to completion events, Start, insert a SystemLivenessProbe
// job, await the resulting completion event, then Stop gracefully.
//
// In addition to the lifecycle assertion (no errors at any stage), the
// test confirms two production-relevant invariants:
//
//   - river_leader carries a row shortly after Start, proving the
//     advisory-lock leader election path landed (covers the
//     single-replica side).
//   - Insert → fetch → Work → complete fires within a generous
//     bound, proving the wiring is functional, not just constructible.
func TestBuildRiverClientLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := migrationtest.Start(ctx)
	if err != nil {
		t.Fatalf("migrationtest.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		h.Stop(stopCtx)
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := store.FromPool(h.Pool)
	client, err := api.BuildRiverClient(api.RiverDeps{
		Pool:   h.Pool,
		Cfg:    config.WorkersConfig{Enabled: true, MaxWorkers: 4},
		Logger: log,
		Store:  s,
	})
	if err != nil {
		t.Fatalf("BuildRiverClient: %v", err)
	}

	// Subscribe before Start so the completion event of the probe we
	// insert below cannot land before the channel is open.
	completions, cancelSub := client.Subscribe(river.EventKindJobCompleted)
	defer cancelSub()

	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// River claims leadership through an advisory-lock-backed insert
	// into river_leader; give the goroutine a moment to land it.
	deadline := time.Now().Add(2 * time.Second)
	var leaderCount int
	for time.Now().Before(deadline) {
		if err := h.Pool.QueryRow(ctx, `select count(*) from river_leader`).Scan(&leaderCount); err != nil {
			t.Fatalf("query river_leader: %v", err)
		}
		if leaderCount > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if leaderCount == 0 {
		t.Fatalf("river_leader stayed empty for 2s after Start; expected at least 1 leader row")
	}

	if err := api.InsertLivenessProbe(ctx, client); err != nil {
		t.Fatalf("InsertLivenessProbe: %v", err)
	}

	// Production wiring also registers periodic workers
	// (heartbeat.reconcile, etc.) that schedule themselves on Start,
	// so other Kinds can race into `completions` ahead of our probe.
	// Filter for the probe kind and treat anything else as background
	// noise so this assertion stays specific to the wiring being tested.
	probeDeadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-completions:
			if ev.Job.Kind == "system.liveness_probe" {
				goto probeCompleted
			}
		case <-probeDeadline:
			t.Fatalf("liveness probe did not complete within 5s")
		}
	}
probeCompleted:

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := client.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestBuildRiverClientValidate covers the per-field
// validation-on-construction contract on RiverDeps. Each row pins one
// nil-or-zero precondition that must error out at boot — surfacing
// the misconfiguration up front beats silently running a queue that
// processes nothing or panics later.
//
// ScanExecutor / ImportExecutor / AgentClient are intentionally
// absent: nil is now an accepted shape (worker simply isn't
// registered) so they are no longer validated.
func TestBuildRiverClientValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		deps api.RiverDeps
	}{
		{
			name: "nil pool",
			deps: api.RiverDeps{
				Pool:   nil,
				Cfg:    config.WorkersConfig{MaxWorkers: 1},
				Logger: slog.Default(),
				Store:  &store.Store{},
			},
		},
		{
			name: "nil store",
			deps: api.RiverDeps{
				Pool:   &pgxpool.Pool{},
				Cfg:    config.WorkersConfig{MaxWorkers: 1},
				Logger: slog.Default(),
				Store:  nil,
			},
		},
		{
			name: "zero max workers",
			deps: api.RiverDeps{
				Pool:   &pgxpool.Pool{},
				Cfg:    config.WorkersConfig{MaxWorkers: 0},
				Logger: slog.Default(),
				Store:  &store.Store{},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := api.BuildRiverClient(tc.deps); err == nil {
				t.Fatalf("BuildRiverClient(%s) returned nil error, want non-nil", tc.name)
			}
		})
	}
}
