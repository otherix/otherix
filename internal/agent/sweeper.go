// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
)

// artifactSweepInterval is how often the agent reconciles its content-addressed
// store against itself (clearing interrupted-upload temp files and repairing or
// dropping sidecarless/corrupt blobs).
const artifactSweepInterval = time.Hour

// artifactStagingMaxAge is the minimum age a staging temp file must reach on a
// periodic sweep before it is removed, so a live upload mid-write is never
// deleted. The boot sweep ignores this (nothing is in flight at boot) and clears
// all staging.
const artifactStagingMaxAge = time.Hour

// artifactSweeper periodically reconciles the node's artifact store against
// itself: it clears interrupted-upload staging temp files and repairs or removes
// blobs whose checksum sidecar is missing (re-hashing to tell a sidecar-write
// crash from real corruption). Purely node-local hygiene; it makes no cluster
// decisions.
type artifactSweeper struct {
	store *artifactstore.Store
	log   *slog.Logger
}

func newArtifactSweeper(store *artifactstore.Store, log *slog.Logger) *artifactSweeper {
	return &artifactSweeper{store: store, log: log}
}

// Run sweeps once immediately (boot pass: clear all staging), then on each tick
// until ctx is cancelled (periodic pass: spare fresh staging). Returns ctx.Err()
// on cancellation so the reconciler drain logs it at info.
func (a *artifactSweeper) Run(ctx context.Context) error {
	a.sweep(ctx, 0)
	t := time.NewTicker(artifactSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			a.sweep(ctx, artifactStagingMaxAge)
		}
	}
}

func (a *artifactSweeper) sweep(ctx context.Context, stagingMaxAge time.Duration) {
	staged, err := a.store.SweepStaging(stagingMaxAge)
	if err != nil {
		a.log.WarnContext(ctx, "artifact sweep: staging sweep failed", slog.Any("error", err))
	}
	repaired, deleted, err := a.store.SweepSidecarless()
	if err != nil {
		a.log.WarnContext(ctx, "artifact sweep: sidecar sweep failed", slog.Any("error", err))
	}
	if staged > 0 || repaired > 0 || deleted > 0 {
		a.log.InfoContext(ctx, "artifact sweep",
			slog.Int("staging_removed", staged),
			slog.Int("sidecars_repaired", repaired),
			slog.Int("blobs_deleted", deleted))
	}
}
