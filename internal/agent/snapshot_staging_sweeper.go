// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"log/slog"
	"time"
)

// snapshotStagingSweepInterval is how often the agent clears orphaned snapshot
// staging temps left under each pool's snapshots/.staging dir.
const snapshotStagingSweepInterval = time.Hour

// snapshotStagingMaxAge is the minimum age a snapshot staging temp must reach on
// a periodic sweep before it is removed. The boot sweep (maxAge 0) is the
// primary cleaner and clears all staging while nothing is in flight; the
// periodic sweep is a backstop for temps a boot sweep never saw. Its max-age is
// set generously, well beyond any expected capture duration, so a slow-but-live
// multi-gigabyte capture's staging temp is never removed mid-write.
const snapshotStagingMaxAge = 24 * time.Hour

// snapshotStagingSweeper clears orphaned snapshot staging temp files left under
// each registered pool's snapshots/.staging dir by an interrupted capture (an
// agent kill or power loss between materializing a temp and the normal-path
// remove). Purely node-local hygiene; it makes no cluster decisions and never
// touches durable content-addressed snapshot blobs.
type snapshotStagingSweeper struct {
	sweep func(maxAge time.Duration) (int, error)
	log   *slog.Logger
}

func newSnapshotStagingSweeper(sweep func(time.Duration) (int, error), log *slog.Logger) *snapshotStagingSweeper {
	return &snapshotStagingSweeper{sweep: sweep, log: log}
}

// BootSweep runs the one-time boot pass synchronously (clear all snapshot
// staging) before the agent starts serving, so a capture that opens a staging
// temp after serving begins is never swept mid-write.
func (s *snapshotStagingSweeper) BootSweep(ctx context.Context) { s.runPass(ctx, 0) }

// Run sweeps on each tick until ctx is cancelled (periodic pass: spare fresh
// staging). The one-time boot pass that clears all staging is run separately via
// BootSweep before serving, so Run performs the periodic pass only. Returns
// ctx.Err() on cancellation so the reconciler drain logs it at info.
func (s *snapshotStagingSweeper) Run(ctx context.Context) error {
	t := time.NewTicker(snapshotStagingSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.runPass(ctx, snapshotStagingMaxAge)
		}
	}
}

func (s *snapshotStagingSweeper) runPass(ctx context.Context, maxAge time.Duration) {
	removed, err := s.sweep(maxAge)
	if err != nil {
		s.log.WarnContext(ctx, "snapshot staging sweep: errors on one or more pools",
			slog.Any("error", err))
	}
	if removed > 0 {
		s.log.InfoContext(ctx, "snapshot staging sweep",
			slog.Int("staging_removed", removed))
	}
}
