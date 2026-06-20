// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactstore

import (
	"context"
	"os"
	"sort"
	"time"
)

// ScrubOptions configures one Scrub pass.
type ScrubOptions struct {
	// MinReverifyInterval is the minimum age (by sidecar mtime) a blob must
	// reach before it is re-verified again. A blob verified more recently is
	// skipped this pass.
	MinReverifyInterval time.Duration
	// MaxBytesPerPass caps the total blob bytes re-hashed in one pass (read I/O
	// budget). At least one eligible blob is always processed so a blob larger
	// than the budget cannot starve.
	MaxBytesPerPass int64
	// TryLock, when non-nil, is taken per victim before re-hashing/deleting; a
	// false return skips the blob (it is being cloned/stored right now). Nil
	// means no lock (the artifact store relies on unlink-during-open safety and
	// the recreate path's blob_unavailable retry). The image store passes
	// Manager.TryLockImageBlob.
	TryLock func(digest string) (release func(), ok bool)
	// Now supplies the clock (injected for tests). Defaults to time.Now.
	Now func() time.Time
}

// ScrubResult reports one pass's outcome.
type ScrubResult struct {
	Verified       int   // blobs re-hashed and confirmed matching their content address
	CorruptDeleted int   // blobs whose content no longer hashed to their name, deleted
	BytesRead      int64 // total blob bytes re-hashed this pass
}

// Scrub re-hashes the least-recently-verified blobs (those whose sidecar mtime
// is older than opts.MinReverifyInterval), coldest first, until opts.MaxBytesPerPass
// bytes have been re-hashed. A blob whose content still hashes to its name has
// its sidecar mtime bumped (the "verified now" memo). A blob whose content no
// longer matches is deleted - the SweepSidecarless precedent - so it drops from
// the heartbeat inventory and the durability reconcile re-replicates a healthy
// copy. Corruption is confirmed ONLY by a clean full read: any I/O error skips
// the blob (fail toward inaction). A sidecar-less blob is skipped here
// (SweepSidecarless owns those). Best-effort per blob.
func (s *Store) Scrub(ctx context.Context, opts ScrubOptions) (ScrubResult, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	if opts.MinReverifyInterval <= 0 || opts.MaxBytesPerPass <= 0 {
		return ScrubResult{}, nil // disabled
	}
	entries, err := s.List()
	if err != nil {
		return ScrubResult{}, err
	}
	cutoff := now().Add(-opts.MinReverifyInterval)

	cands := make([]scrubCand, 0, len(entries))
	for _, e := range entries {
		fi, serr := os.Stat(s.sidecarPath(e.Digest))
		if serr != nil {
			continue // no sidecar (SweepSidecarless handles those) or unreadable
		}
		if !fi.ModTime().Before(cutoff) {
			continue // verified recently enough
		}
		cands = append(cands, scrubCand{digest: e.Digest, size: e.SizeBytes, mtime: fi.ModTime()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime.Before(cands[j].mtime) })

	var res ScrubResult
	for _, c := range cands {
		if res.BytesRead >= opts.MaxBytesPerPass {
			break
		}
		if ctx.Err() != nil {
			return res, nil
		}
		verified, deleted, n := s.scrubOne(c, opts.TryLock, now)
		switch {
		case verified:
			res.Verified++
		case deleted:
			res.CorruptDeleted++
		}
		res.BytesRead += n
	}
	return res, nil
}

// scrubCand is one re-verification candidate: a stored blob whose sidecar mtime
// is older than the reverify cutoff.
type scrubCand struct {
	digest string
	size   int64
	mtime  time.Time
}

// scrubOne re-hashes a single candidate under the optional per-blob lock,
// bumping the sidecar mtime on a match (the verified-now memo) or deleting on
// confirmed corruption (a clean read whose content no longer hashes to its
// name). An I/O error or lock contention is a no-op (fail toward inaction). It
// reports whether the blob was verified, whether it was deleted, and the bytes
// read (the candidate size on a processed blob, zero when skipped).
func (s *Store) scrubOne(c scrubCand, tryLock func(digest string) (release func(), ok bool), now func() time.Time) (verified, deleted bool, bytesRead int64) {
	if tryLock != nil {
		release, ok := tryLock(c.digest)
		if !ok {
			return false, false, 0 // being cloned/stored; never delete mid-clone
		}
		defer release()
	}
	ok, herr := s.blobHashesTo(c.digest)
	if herr != nil {
		return false, false, 0 // I/O error: skip, do NOT delete (fail toward inaction)
	}
	if ok {
		t := now()
		_ = os.Chtimes(s.sidecarPath(c.digest), t, t) // mark verified-now
		return true, false, c.size
	}
	_ = s.Delete(c.digest) // confirmed corrupt: drop it so durability self-heals
	return false, true, c.size
}
