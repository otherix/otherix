// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SweepStaging removes stale temp files left under blobs/.staging by an
// interrupted Put. A file is removed when its modification time is older than
// now-maxAge; maxAge == 0 removes every staging file (the boot sweep, when no
// Put is in flight). A positive maxAge spares a temp file a live Put may still
// be writing. Best-effort: a per-file error is skipped, not fatal. Returns the
// count removed. An absent staging dir yields (0, nil).
func (s *Store) SweepStaging(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(s.stagingDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("artifactstore: read staging dir: %v", err)
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if maxAge > 0 {
			info, ierr := e.Info()
			if ierr != nil || !info.ModTime().Before(cutoff) {
				continue
			}
		}
		if err := os.Remove(filepath.Join(s.stagingDir(), e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// SweepSidecarless reconciles blobs and their sidecars under blobs/. For a blob
// (a regular file whose name is a hex sha256) with no sidecar it re-hashes the
// content: a hash equal to the filename means a valid blob whose sidecar write
// was interrupted, so the sidecar is rewritten (the blob is kept); a hash that
// differs means the file is corrupt and unusable, so it is deleted. A sidecar
// with no backing blob is an orphan and is deleted, but only after re-checking
// the blob is still absent (a concurrent upload may have landed it after the
// directory listing). A non-hex file name is left alone. Best-effort per entry.
// Returns (repaired, deleted) counts.
func (s *Store) SweepSidecarless() (int, int, error) {
	entries, err := os.ReadDir(s.blobsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("artifactstore: read blobs dir: %v", err)
	}
	blobs, sidecars := partitionBlobDir(entries)
	repaired, deleted := s.repairSidecarlessBlobs(blobs, sidecars)
	deleted += s.dropOrphanSidecars(blobs, sidecars)
	return repaired, deleted, nil
}

// partitionBlobDir splits a blobs/ directory listing into the set of blob digests
// (regular files whose name is a hex sha256) and the set of sidecar digests
// (files ending .sha256). The .staging subdir and other entries are ignored.
func partitionBlobDir(entries []os.DirEntry) (blobs, sidecars map[string]struct{}) {
	blobs, sidecars = map[string]struct{}{}, map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if d, ok := strings.CutSuffix(name, ".sha256"); ok {
			sidecars[d] = struct{}{}
			continue
		}
		if isHexSHA256(name) {
			blobs[name] = struct{}{}
		}
	}
	return blobs, sidecars
}

// repairSidecarlessBlobs re-hashes each blob lacking a sidecar: a content match
// rewrites the sidecar (the blob is kept), a mismatch deletes the corrupt file.
// Best-effort per blob. Returns (repaired, deleted).
func (s *Store) repairSidecarlessBlobs(blobs, sidecars map[string]struct{}) (int, int) {
	repaired, deleted := 0, 0
	for digest := range blobs {
		if _, ok := sidecars[digest]; ok {
			continue
		}
		ok, herr := s.blobHashesTo(digest)
		if herr != nil {
			continue
		}
		if ok {
			if err := os.WriteFile(s.sidecarPath(digest), []byte(digest), 0o644); err == nil { //nolint:gosec // sidecar is non-secret metadata
				repaired++
			}
			continue
		}
		if err := os.Remove(s.blobPath(digest)); err == nil {
			deleted++
		}
	}
	return repaired, deleted
}

// dropOrphanSidecars deletes each sidecar whose blob is absent both from the
// listing and at delete time (the re-check spares a sidecar a concurrent upload
// just landed). Best-effort. Returns the count deleted.
func (s *Store) dropOrphanSidecars(blobs, sidecars map[string]struct{}) int {
	deleted := 0
	for digest := range sidecars {
		if _, ok := blobs[digest]; ok {
			continue
		}
		if s.Has(digest) {
			continue
		}
		if err := os.Remove(s.sidecarPath(digest)); err == nil {
			deleted++
		}
	}
	return deleted
}

// blobHashesTo re-hashes the blob file for digest and reports whether its sha256
// equals digest (i.e. the content matches its content-address).
func (s *Store) blobHashesTo(digest string) (bool, error) {
	f, err := os.Open(s.blobPath(digest)) //nolint:gosec // digest is validated hex; path is store-internal
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == digest, nil
}
