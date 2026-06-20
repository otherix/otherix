// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/config"
)

// imageCacheSweeper bounds the node-level pinned-image cache store. Each pass
// evicts the coldest cached images (LRU by file mtime) until the store is under
// its byte ceiling AND its partition is above the free-space floor. It is the one
// destructive operation in the image tier, fenced three ways: it deletes only
// image-store blobs, only a victim whose per-digest lock it can take (so an image
// being cloned is never deleted), and only as much as the ceilings require.
type imageCacheSweeper struct {
	store     *artifactstore.Store
	tryLock   func(digest string) (release func(), ok bool)
	freeBytes func(path string) (uint64, error)
	cfg       config.ImageCacheConfig
	nudge     <-chan struct{}
	// afterPass runs at the end of every sweep, after any eviction. It reclaims
	// orphaned <digest>.meta sidecars whose blob this pass (or a scrub) removed.
	// Best-effort and nil-safe; an eviction pass is exactly when a meta orphans.
	afterPass func()
	log       *slog.Logger
}

func newImageCacheSweeper(store *artifactstore.Store, tryLock func(string) (func(), bool), freeBytes func(string) (uint64, error), cfg config.ImageCacheConfig, nudge <-chan struct{}, afterPass func(), log *slog.Logger) *imageCacheSweeper {
	return &imageCacheSweeper{store: store, tryLock: tryLock, freeBytes: freeBytes, cfg: cfg, nudge: nudge, afterPass: afterPass, log: log}
}

// Run sweeps once at boot, then on each tick or nudge, until ctx is done.
func (s *imageCacheSweeper) Run(ctx context.Context) error {
	interval := s.cfg.EvictionInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	s.pass(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.pass(ctx)
		case <-s.nudge:
			s.pass(ctx)
		}
	}
}

// pass runs one eviction pass and then the best-effort orphaned-meta sweep, so a
// meta whose blob this pass (or a scrub) removed is reclaimed on the same tick.
func (s *imageCacheSweeper) pass(ctx context.Context) {
	s.sweep(ctx)
	if s.afterPass != nil {
		s.afterPass()
	}
}

func (s *imageCacheSweeper) sweep(ctx context.Context) {
	if s.cfg.MaxBytes <= 0 && s.cfg.MinFreeBytes <= 0 {
		return // eviction disabled
	}
	entries, err := s.store.List()
	if err != nil {
		s.log.Warn("image cache eviction: list failed, skipping pass", "error", err.Error())
		return
	}
	need := s.bytesToReclaim(entries)
	if need <= 0 {
		return
	}
	cands := s.coldestFirst(entries)

	var reclaimed, evicted int64
	for _, c := range cands {
		if reclaimed >= need {
			break
		}
		if ctx.Err() != nil {
			return
		}
		release, ok := s.tryLock(c.digest)
		if !ok {
			continue // a create is cloning/storing this digest; never delete mid-clone
		}
		delErr := s.store.Delete(c.digest)
		release()
		if delErr != nil {
			s.log.Warn("image cache eviction: delete failed", "digest", c.digest, "error", delErr.Error())
			continue
		}
		reclaimed += c.size
		evicted++
	}
	s.log.Info("image cache eviction pass", "evicted", evicted, "bytes_reclaimed", reclaimed, "bytes_needed", need)
	if reclaimed < need {
		s.log.Warn("image cache eviction could not reach target",
			"bytes_reclaimed", reclaimed, "bytes_needed", need,
			"reason", "all remaining candidates locked or non-image partition pressure")
	}
}

// bytesToReclaim returns how many bytes this pass must evict to bring the store
// under MaxBytes AND its partition above MinFreeBytes, taking the larger of the
// two deficits. A statfs error fails open: the free-space floor is skipped for
// the pass rather than treated as zero free.
func (s *imageCacheSweeper) bytesToReclaim(entries []artifactstore.BlobEntry) int64 {
	var total int64
	for _, e := range entries {
		total += e.SizeBytes
	}
	var need int64
	if s.cfg.MaxBytes > 0 && total > s.cfg.MaxBytes {
		need = total - s.cfg.MaxBytes
	}
	if s.cfg.MinFreeBytes <= 0 {
		return need
	}
	free, err := s.freeBytes(s.store.Root())
	if err != nil {
		s.log.Warn("image cache eviction: statfs failed, skipping free-space floor", "error", err.Error())
		return need
	}
	// MinFreeBytes is positive here, so the conversion to uint64 is well-defined;
	// the comparison and subtraction stay in uint64 and never exceed MinFreeBytes.
	floor := uint64(s.cfg.MinFreeBytes)
	if free < floor {
		if deficit := int64(floor - free); deficit > need { //nolint:gosec // G115: free < floor, so floor-free <= floor = uint64(MinFreeBytes), always a valid positive int64
			need = deficit
		}
	}
	return need
}

// imageCacheCand is one eviction candidate: a stored blob plus the file mtime
// used to order coldest-first.
type imageCacheCand struct {
	digest string
	size   int64
	mtime  time.Time
}

// coldestFirst returns the candidates sorted by blob-file mtime ascending
// (LRU, coldest first). A blob that fails to stat (e.g. concurrently deleted)
// is dropped from the candidate set.
func (s *imageCacheSweeper) coldestFirst(entries []artifactstore.BlobEntry) []imageCacheCand {
	cands := make([]imageCacheCand, 0, len(entries))
	for _, e := range entries {
		p, perr := s.store.BlobPath(e.Digest)
		if perr != nil {
			continue
		}
		fi, serr := os.Stat(p)
		if serr != nil {
			continue
		}
		cands = append(cands, imageCacheCand{digest: e.Digest, size: e.SizeBytes, mtime: fi.ModTime()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime.Before(cands[j].mtime) })
	return cands
}
