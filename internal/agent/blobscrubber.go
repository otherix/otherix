// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/config"
)

// blobScrubber periodically re-hashes the node's stored blobs to catch silent
// corruption (bit-rot) a Put-time verify cannot, deleting a confirmed-corrupt
// copy so the heartbeat inventory + durability reconcile re-replicate a healthy
// one. It runs over both content-addressed stores: the durability artifact store
// and the pinned-image cache. The image-store pass holds the per-digest image
// lock (never delete mid-clone); the artifact-store pass needs no lock (unlink is
// safe during an open read, and the recreate path retries on blob_unavailable).
type blobScrubber struct {
	artifactStore *artifactstore.Store
	imageStore    *artifactstore.Store                          // nil when no artifacts root
	imageTryLock  func(digest string) (release func(), ok bool) // Manager.TryLockImageBlob; nil when no image store
	cfg           config.ScrubConfig
	log           *slog.Logger
}

// Run sweeps once at boot, then on each tick until ctx is cancelled. A disabled
// config (any of the three knobs zero) returns immediately.
func (s *blobScrubber) Run(ctx context.Context) error {
	if s.cfg.Interval <= 0 || s.cfg.MinReverifyInterval <= 0 || s.cfg.MaxBytesPerPass <= 0 {
		s.log.Info("blob scrubber disabled (zero interval/reverify/budget)")
		return nil
	}
	s.sweep(ctx)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *blobScrubber) sweep(ctx context.Context) {
	opts := artifactstore.ScrubOptions{
		MinReverifyInterval: s.cfg.MinReverifyInterval,
		MaxBytesPerPass:     s.cfg.MaxBytesPerPass,
		Now:                 time.Now,
	}
	if s.artifactStore != nil {
		res, err := s.artifactStore.Scrub(ctx, opts)
		if err != nil {
			s.log.WarnContext(ctx, "blob scrub: artifact store pass failed", slog.Any("error", err))
		} else if res.CorruptDeleted > 0 || res.Verified > 0 {
			s.log.InfoContext(ctx, "blob scrub: artifact store",
				slog.Int("verified", res.Verified), slog.Int("corrupt_deleted", res.CorruptDeleted))
		}
	}
	if s.imageStore != nil {
		imgOpts := opts
		imgOpts.TryLock = s.imageTryLock
		res, err := s.imageStore.Scrub(ctx, imgOpts)
		if err != nil {
			s.log.WarnContext(ctx, "blob scrub: image store pass failed", slog.Any("error", err))
		} else if res.CorruptDeleted > 0 || res.Verified > 0 {
			s.log.InfoContext(ctx, "blob scrub: image store",
				slog.Int("verified", res.Verified), slog.Int("corrupt_deleted", res.CorruptDeleted))
		}
	}
}
