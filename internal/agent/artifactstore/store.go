// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package artifactstore is the agent-local, content-addressed blob store: one
// store per node, independent of the disk pools. It holds immutable
// full-copy qcow2 blobs keyed by their sha256 digest plus the snapshot/image
// manifests, and is the backing store for both snapshot capture and the
// cross-node pull. Put verifies the digest before a blob becomes
// visible (stage -> hash -> atomic rename -> sidecar), so an unverified or
// short-read blob is never exposed.
package artifactstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// allowedRootPrefix is the fixed allowlist the production Root must sit under,
// mirroring config.artifactsRootAllowedPrefix and storage_pools' path allowlist.
// Keep in sync with config.artifactsRootAllowedPrefix.
const allowedRootPrefix = "/var/lib/otherix/"

// Sentinel errors. ErrDigestMismatch is returned by Put when the streamed bytes
// do not hash to the claimed digest (fail-closed: the blob is discarded, never
// renamed into place). ErrNotFound is returned by Get/ReadManifest for an
// absent digest. ErrInvalidDigest rejects a non-hex-sha256 key before it is
// concatenated into a filesystem path (defense in depth against traversal).
var (
	ErrDigestMismatch = errors.New("artifactstore: streamed content does not match claimed digest")
	ErrNotFound       = errors.New("artifactstore: blob not found")
	ErrInvalidDigest  = errors.New("artifactstore: digest is not a lowercase hex sha256")
)

// Store is one node's content-addressed blob store rooted at root.
type Store struct {
	root string
}

// New constructs a Store rooted at root, rejecting a root outside the fixed
// allowlist prefix (/var/lib/otherix/) and creating the blobs / manifests /
// staging directories. Production callers pass the configured artifacts.root.
func New(root string) (*Store, error) {
	clean := filepath.Clean(root)
	if !strings.HasPrefix(clean+"/", allowedRootPrefix) {
		return nil, fmt.Errorf("artifactstore: root %q must be under %s", root, allowedRootPrefix)
	}
	return newForTest(clean)
}

// newForTest constructs a Store rooted at root WITHOUT the allowlist check, for
// unit tests using t.TempDir(). Not exported; production goes through New.
func newForTest(root string) (*Store, error) {
	s := &Store{root: filepath.Clean(root)}
	for _, dir := range []string{s.blobsDir(), s.manifestsDir(), s.stagingDir()} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("artifactstore: mkdir %s: %v", dir, err)
		}
	}
	return s, nil
}

// NewForTesting constructs a Store rooted at root without the allowlist check.
// For tests in OTHER packages that need a store on a t.TempDir() path; production
// uses New.
func NewForTesting(root string) (*Store, error) { return newForTest(root) }

func (s *Store) blobsDir() string     { return filepath.Join(s.root, "blobs") }
func (s *Store) manifestsDir() string { return filepath.Join(s.root, "manifests") }
func (s *Store) stagingDir() string   { return filepath.Join(s.blobsDir(), ".staging") }

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.blobsDir(), digest)
}

func (s *Store) sidecarPath(digest string) string {
	return filepath.Join(s.blobsDir(), digest+".sha256")
}

func (s *Store) manifestPath(digest string) string {
	return filepath.Join(s.manifestsDir(), digest)
}

// isHexSHA256 reports whether d is a 64-char lowercase hex string (a sha256
// content address). Rejects '/'-bearing or short/long values so a malformed
// digest is never concatenated into a path.
func isHexSHA256(d string) bool {
	if len(d) != 64 {
		return false
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Has reports whether the blob for digest is present (a well-formed digest with
// a regular file under blobs/). A malformed digest reports false.
func (s *Store) Has(digest string) bool {
	if !isHexSHA256(digest) {
		return false
	}
	info, err := os.Stat(s.blobPath(digest))
	return err == nil && info.Mode().IsRegular()
}

// Get opens the blob for digest for reading. Returns ErrNotFound when absent or
// ErrInvalidDigest for a malformed digest. The caller closes the reader.
func (s *Store) Get(digest string) (io.ReadCloser, error) {
	if !isHexSHA256(digest) {
		return nil, ErrInvalidDigest
	}
	f, err := os.Open(s.blobPath(digest)) //nolint:gosec // digest is validated hex; path is store-internal
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("artifactstore: open %s: %v", digest, err)
	}
	return f, nil
}

// Put streams r into the store under digest: it writes to a staging file,
// computes the sha256 on the way, and only if the hash equals digest does it
// atomically rename the staging file into blobs/{digest} and write the sidecar.
// A digest already present is an idempotent no-op success (content-addressed
// dedup). A hash mismatch discards the staging file and returns
// ErrDigestMismatch - the blob is never made visible.
func (s *Store) Put(digest string, r io.Reader) error {
	if !isHexSHA256(digest) {
		return ErrInvalidDigest
	}
	if s.Has(digest) {
		return nil
	}
	if err := os.MkdirAll(s.stagingDir(), 0o750); err != nil {
		return fmt.Errorf("artifactstore: mkdir staging: %v", err)
	}
	tmp := filepath.Join(s.stagingDir(), uuid.NewString())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) //nolint:gosec // staging path is store-internal, fresh uuid
	if err != nil {
		return fmt.Errorf("artifactstore: create staging: %v", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), r); err != nil {
		cleanup()
		return fmt.Errorf("artifactstore: stream blob: %v", err)
	}
	// fsync the staged data before the rename so the bytes are durable before
	// the rename names them: on a power loss the rename must never become
	// visible ahead of the data blocks (which would leave a valid-named blob
	// with garbage content that List advertises as a healthy holder).
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("artifactstore: sync staging: %v", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("artifactstore: close staging: %v", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != digest {
		_ = os.Remove(tmp)
		return fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, got, digest)
	}
	if s.Has(digest) {
		_ = os.Remove(tmp)
		return nil
	}
	if err := os.Rename(tmp, s.blobPath(digest)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("artifactstore: atomic rename: %v", err)
	}
	// fsync the blobs dir so the rename itself is durable. On a sync failure
	// the blob is on disk but not durably linked; surface the error so the
	// caller retries rather than falsely reporting success.
	if err := syncDir(s.blobsDir()); err != nil {
		return err
	}
	if err := os.WriteFile(s.sidecarPath(digest), []byte(digest), 0o644); err != nil { //nolint:gosec // sidecar is non-secret metadata
		return fmt.Errorf("artifactstore: write sidecar: %v", err)
	}
	// fsync the blobs dir again so the sidecar entry is durable.
	if err := syncDir(s.blobsDir()); err != nil {
		return err
	}
	return nil
}

// syncDir fsyncs a directory so a rename/create within it is durable across a
// power loss. Best-effort callers wrap the error; a sync failure on the parent
// dir is reported, not swallowed, since it defeats the crash-durability guarantee.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- store-internal directory path
	if err != nil {
		return fmt.Errorf("artifactstore: open dir for sync: %v", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("artifactstore: sync dir: %v", err)
	}
	return d.Close()
}

// Delete removes the blob and its sidecar for digest. An absent blob is a no-op
// (not an error). A malformed digest is rejected.
func (s *Store) Delete(digest string) error {
	if !isHexSHA256(digest) {
		return ErrInvalidDigest
	}
	if err := os.Remove(s.blobPath(digest)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("artifactstore: remove blob %s: %v", digest, err)
	}
	if err := os.Remove(s.sidecarPath(digest)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("artifactstore: remove sidecar %s: %v", digest, err)
	}
	return nil
}

// WriteManifest atomically writes the manifest body keyed by digest.
func (s *Store) WriteManifest(digest string, body []byte) error {
	if !isHexSHA256(digest) {
		return ErrInvalidDigest
	}
	if err := os.MkdirAll(s.manifestsDir(), 0o750); err != nil {
		return fmt.Errorf("artifactstore: mkdir manifests: %v", err)
	}
	tmp := filepath.Join(s.manifestsDir(), "."+uuid.NewString()+".tmp")
	if err := writeFileSync(tmp, body, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("artifactstore: write staging manifest: %v", err)
	}
	if err := os.Rename(tmp, s.manifestPath(digest)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("artifactstore: rename manifest: %v", err)
	}
	// fsync the manifests dir so the rename itself is durable across a power loss.
	if err := syncDir(s.manifestsDir()); err != nil {
		return err
	}
	return nil
}

// writeFileSync writes body to path and fsyncs it before close, so the bytes
// are durable before a subsequent rename names the file. Mirrors Put's
// staged-file sync discipline for the manifest path.
func writeFileSync(path string, body []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) // #nosec G304 -- store-internal staging path, fresh uuid
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadManifest returns the manifest body keyed by digest, or ErrNotFound.
func (s *Store) ReadManifest(digest string) ([]byte, error) {
	if !isHexSHA256(digest) {
		return nil, ErrInvalidDigest
	}
	body, err := os.ReadFile(s.manifestPath(digest)) //nolint:gosec // digest is validated hex; path is store-internal
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("artifactstore: read manifest %s: %v", digest, err)
	}
	return body, nil
}

// BlobEntry is one stored blob's content address + byte size, returned by List.
type BlobEntry struct {
	Digest    string
	SizeBytes int64
}

// List enumerates every present blob (a regular file under blobs/ whose name is
// a hex sha256) with its size, ordered by digest. Sidecars and the .staging
// dir are skipped. An absent blobs dir yields an empty slice and nil error.
func (s *Store) List() ([]BlobEntry, error) {
	entries, err := os.ReadDir(s.blobsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifactstore: read blobs dir: %v", err)
	}
	out := make([]BlobEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".sha256") {
			continue
		}
		if !isHexSHA256(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BlobEntry{Digest: e.Name(), SizeBytes: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

// Root returns the store's root path (used by the relocation sweep and tests).
func (s *Store) Root() string { return s.root }

// BlobPath returns the on-disk path of the blob for digest WITHOUT checking
// presence. The recreate dual-read uses it (after Has) to re-hash + full-copy
// clone the store-resident blob into a fresh VM disk, reusing the same
// verify-then-clone path the disk-pool source takes. Returns ErrInvalidDigest
// for a malformed digest so a bad key is never concatenated into a path.
func (s *Store) BlobPath(digest string) (string, error) {
	if !isHexSHA256(digest) {
		return "", ErrInvalidDigest
	}
	return s.blobPath(digest), nil
}
