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
// periodic sweep before it is removed. The boot sweep (maxAge 0) is the primary
// cleaner and clears all staging while nothing is in flight; the periodic sweep
// is only a backstop for staging temps a boot sweep never saw. Its max-age is
// therefore set generously, well beyond any expected blob-upload duration, so a
// slow-but-live upload's staging temp is never removed mid-write.
const artifactStagingMaxAge = 24 * time.Hour

// artifactSweeper periodically reconciles the node's artifact store against
// itself: it clears interrupted-upload staging temp files and repairs or removes
// blobs whose checksum sidecar is missing (re-hashing to tell a sidecar-write
// crash from real corruption). Purely node-local hygiene; it makes no cluster
// decisions.
type artifactSweeper struct {
	store      *artifactstore.Store
	imageStore *artifactstore.Store // node image store; nil when no artifact root (test path)
	log        *slog.Logger
}

func newArtifactSweeper(store, imageStore *artifactstore.Store, log *slog.Logger) *artifactSweeper {
	return &artifactSweeper{store: store, imageStore: imageStore, log: log}
}

// BootSweep runs the one-time boot pass synchronously (clear all staging on both
// stores) before the agent starts serving, so an in-flight Put opened after
// serving begins is never swept mid-write.
func (a *artifactSweeper) BootSweep(ctx context.Context) { a.sweep(ctx, 0) }

// Run sweeps on each tick until ctx is cancelled (periodic pass: spare fresh
// staging). The one-time boot pass that clears all staging is run separately via
// BootSweep before serving, so Run performs the periodic pass only. Returns
// ctx.Err() on cancellation so the reconciler drain logs it at info.
func (a *artifactSweeper) Run(ctx context.Context) error {
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
	a.sweepStore(ctx, "artifact store", a.store, stagingMaxAge)
	if a.imageStore != nil {
		a.sweepStore(ctx, "image store", a.imageStore, stagingMaxAge)
	}
}

func (a *artifactSweeper) sweepStore(ctx context.Context, label string, store *artifactstore.Store, stagingMaxAge time.Duration) {
	staged, err := store.SweepStaging(stagingMaxAge)
	if err != nil {
		a.log.WarnContext(ctx, "artifact sweep: staging sweep failed",
			slog.String("store", label), slog.Any("error", err))
	}
	repaired, deleted, err := store.SweepSidecarless()
	if err != nil {
		a.log.WarnContext(ctx, "artifact sweep: sidecar sweep failed",
			slog.String("store", label), slog.Any("error", err))
	}
	if staged > 0 || repaired > 0 || deleted > 0 {
		a.log.InfoContext(ctx, "artifact sweep",
			slog.String("store", label),
			slog.Int("staging_removed", staged),
			slog.Int("sidecars_repaired", repaired),
			slog.Int("blobs_deleted", deleted))
	}
}
