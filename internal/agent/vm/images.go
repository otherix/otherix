// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// importHTTPTimeout caps the worst-case duration of a single image
// download. 1h is the agent-side default; ROADMAP entry
// "agent.storage.dir.import_timeout config" tracks operator-facing
// exposure.
const importHTTPTimeout = time.Hour

// importMaxRedirects bounds redirect chain length for source URL
// fetches.
const importMaxRedirects = 5

// imagesSubdir is the per-pool cache directory holding the image files and
// their sidecars (renamed from the former templates/ layout).
const imagesSubdir = "images"

// Image-surface sentinel errors. Callers branch on errors.Is for envelope
// mapping; EnsureImage is synchronous so all of these surface directly to
// the create flow that drives it.
var (
	// ErrUnsupportedFormat is returned when the requested format is not
	// "qcow2". `raw` is a deferred enum extension.
	ErrUnsupportedFormat = errors.New("unsupported image format (qcow2 only)")
	// ErrMissingSourceURL is returned when the source URL is empty. Only
	// source_url is accepted; source_path not implemented (separate
	// iteration tracked in ROADMAP).
	ErrMissingSourceURL = errors.New("source_url is required")
	// ErrInvalidChecksumFormat is returned when a non-empty expected
	// checksum fails the 64-char lowercase hex pattern.
	ErrInvalidChecksumFormat = errors.New("expected_checksum_sha256 must be 64-char lowercase hex")
)

// ChecksumMismatchError signals the URL did not serve the operator-pinned
// sha256 (verify mode). The agent writes nothing into the cache and the
// create fails.
type ChecksumMismatchError struct {
	Expected string
	Actual   string
	URL      string
}

func (e *ChecksumMismatchError) Error() string {
	return fmt.Sprintf("checksum_mismatch: url %s served sha %s, expected %s", e.URL, e.Actual, e.Expected)
}

// EnsureResult is the outcome of EnsureImage: the resolved content digest,
// the on-disk path of the cached file, and its byte size.
type EnsureResult struct {
	SHA256    string
	Path      string
	SizeBytes int64
}

// CachedImage is the agent-internal view of one entry in a pool's image
// cache, projected by ListImages from a filesystem walk of
// {pool.root}/images/. The HTTP handler shapes this to the wire schema.
type CachedImage struct {
	Basename       string
	ChecksumSHA256 string
	Format         string
	SizeBytes      int64
	ImportedAt     time.Time
}

// imageLockKey indexes the per-(pool, basename) mutex preventing concurrent
// ensures of the same cached image from clobbering each other. The mutex
// stays alive in the map for the lifetime of the manager — cleanup is a
// future iteration concern. The pool dimension is a string name (the
// agent's pool registry is name-keyed).
type imageLockKey struct {
	pool     string
	basename string
}

// lockForImage returns the mutex for (poolName, basename). LoadOrStore keeps
// a stable identity, so two concurrent goroutines see the same *sync.Mutex
// and serialise correctly. Distinct basenames take distinct mutexes and do
// not block each other.
func (m *Manager) lockForImage(poolName, basename string) *sync.Mutex {
	actual, _ := m.imageLocks.LoadOrStore(imageLockKey{pool: poolName, basename: basename}, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	return mu
}

// basenameFromURL derives the cache identity (the URL basename) from a
// source URL, stripping any query string. Mirrors k8s image-tag semantics:
// a basename collision across two distinct URLs reuses the first, which is
// documented operator responsibility.
func basenameFromURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	return path.Base(rawURL)
}

// EnsureImage materializes the image identified by sourceURL on poolName,
// applying the IfNotPresent pull policy keyed by the URL basename. It is
// synchronous: runCreate drives it inline before cloning the resulting
// cached file. The pull policy:
//
//   - no expected sha, basename present  -> HIT (cached sha from sidecar)
//   - no expected sha, basename absent   -> download, compute sha, write
//   - expected sha S, absent             -> download, verify == S, write
//     (ChecksumMismatchError, nothing written, when the URL serves != S)
//   - expected sha S, present, sidecar == S -> HIT
//   - expected sha S, present, sidecar != S -> re-download, verify == S,
//     atomic overwrite (the existing file survives a failed re-download:
//     the rename-in only happens after the verify passes)
//
// Synchronous errors (caller-facing):
//   - ErrPoolUnknown — poolName is not configured
//   - ErrUnsupportedFormat — format != "qcow2"
//   - ErrMissingSourceURL — sourceURL empty
//   - ErrInvalidChecksumFormat — non-empty expectedSHA is not 64-char
//     lowercase hex (empty expectedSHA is allowed: compute mode)
//   - *ChecksumMismatchError — verify mode and the served bytes disagree
//   - wrapped errors for download / qcow2-magic / filesystem failures
func (m *Manager) EnsureImage(ctx context.Context, poolName, sourceURL, expectedSHA, format string) (EnsureResult, error) {
	if format != "qcow2" {
		return EnsureResult{}, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	if sourceURL == "" {
		return EnsureResult{}, ErrMissingSourceURL
	}
	if expectedSHA != "" && !isHexSHA256Lower(expectedSHA) {
		return EnsureResult{}, ErrInvalidChecksumFormat
	}
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return EnsureResult{}, ErrPoolUnknown
	}

	basename := basenameFromURL(sourceURL)
	lock := m.lockForImage(poolName, basename)
	lock.Lock()
	defer lock.Unlock()

	imgPath := filepath.Join(p.root, imagesSubdir, basename)
	sidecarPath := imgPath + ".sha256"

	if cachedSHA, size, present := readCachedImage(imgPath, sidecarPath); present {
		if expectedSHA == "" || cachedSHA == expectedSHA {
			return EnsureResult{SHA256: cachedSHA, Path: imgPath, SizeBytes: size}, nil
		}
		// expectedSHA set and sidecar disagrees: stale slot -> re-download+overwrite.
	}
	return m.downloadIntoCache(ctx, p.root, basename, imgPath, sidecarPath, sourceURL, expectedSHA)
}

// readCachedImage reports the cached sha (from the sidecar), the file size,
// and presence. A file present without a readable, well-formed sidecar is
// treated as absent so the next ensure recomputes both.
func readCachedImage(imgPath, sidecarPath string) (sha string, size int64, present bool) {
	info, err := os.Stat(imgPath)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", 0, false
	}
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		return "", 0, false
	}
	s := strings.TrimSpace(string(raw))
	if !isHexSHA256Lower(s) {
		return "", 0, false
	}
	return s, info.Size(), true
}

// downloadIntoCache streams sourceURL to a scratch file, enforces
// expectedSHA when set, validates the qcow2 magic, then atomically renames
// the file into {root}/images/{basename} and writes the sidecar. On a
// verify-mode mismatch it returns *ChecksumMismatchError and writes nothing
// into the cache — any pre-existing cached file is left untouched because
// the rename-in happens only after the verify passes.
//
// The sha-check runs BEFORE the qcow2-magic check so a URL that disagrees
// with the operator-pinned digest fails closed as checksum_mismatch rather
// than masking the disagreement behind a header error.
func (m *Manager) downloadIntoCache(ctx context.Context, root, basename, imgPath, sidecarPath, sourceURL, expectedSHA string) (EnsureResult, error) {
	scratchDir := filepath.Join(root, "scratch", "import")
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return EnsureResult{}, fmt.Errorf("create scratch dir: %v", err)
	}
	tempPath := filepath.Join(scratchDir, basename+"."+uuid.NewString()+".tmp")
	defer func() { _ = os.Remove(tempPath) }()

	size, computedSHA, dlErr := downloadAndHash(ctx, sourceURL, tempPath)
	if dlErr != nil {
		return EnsureResult{}, fmt.Errorf("download %s: %v", sourceURL, dlErr)
	}
	if expectedSHA != "" && computedSHA != expectedSHA {
		return EnsureResult{}, &ChecksumMismatchError{Expected: expectedSHA, Actual: computedSHA, URL: sourceURL}
	}
	if err := validateQcow2Magic(tempPath); err != nil {
		return EnsureResult{}, fmt.Errorf("qcow2_header_invalid: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(imgPath), 0o750); err != nil {
		return EnsureResult{}, fmt.Errorf("create images dir: %v", err)
	}
	if err := os.Rename(tempPath, imgPath); err != nil {
		return EnsureResult{}, fmt.Errorf("atomic rename to images: %v", err)
	}
	if err := os.WriteFile(sidecarPath, []byte(computedSHA), 0o644); err != nil { //nolint:gosec // sidecar is non-secret metadata
		return EnsureResult{}, fmt.Errorf("write sidecar: %v", err)
	}
	return EnsureResult{SHA256: computedSHA, Path: imgPath, SizeBytes: size}, nil
}

// downloadError carries the source-side HTTP status alongside the underlying
// network error so callers can surface details.status. status==0 means
// transport-level failure (DNS, TLS, connection reset, …) before any
// response status was observed.
type downloadError struct {
	status int
	cause  error
}

func (d *downloadError) Error() string { return d.cause.Error() }
func (d *downloadError) Unwrap() error { return d.cause }

// downloadAndHash streams sourceURL → tempPath while hashing the body in one
// pass. Returns (size_bytes, hex_sha256, nil) on success or
// (0, "", *downloadError) on any failure. HTTP-level failures populate
// downloadError.status; transport failures leave it zero.
func downloadAndHash(ctx context.Context, sourceURL, tempPath string) (int64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, importHTTPTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("build request: %w", err)}
	}

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= importMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("http get: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, "", &downloadError{
			status: resp.StatusCode,
			cause:  fmt.Errorf("source returned HTTP %d", resp.StatusCode),
		}
	}

	// #nosec G304 -- tempPath is constructed inside downloadIntoCache from
	// the pool root (operator-configured) + URL basename + server-minted
	// uuid; no user-controlled component reaches this path unmediated.
	f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("create temp file: %w", err)}
	}

	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(f, hasher), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("stream body: %w", copyErr)}
	}
	if closeErr != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("close temp file: %w", closeErr)}
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// qcow2Magic is the first 4 bytes of every valid qcow2 image: 'QFI\xfb'
// (0x514649fb big-endian). Magic-only validation; full 9-rule header
// validation deferred — ROADMAP entry "full qcow2 header validation".
var qcow2Magic = [4]byte{'Q', 'F', 'I', 0xfb}

// validateQcow2Magic reads the first 4 bytes of path and rejects anything
// other than qcow2Magic. Magic-only validation; the remaining 8 header
// rules (version, backing_file, crypt_method, cluster_bits, virtual_size,
// v3 extensions) ride on the deferred work item.
func validateQcow2Magic(path string) error {
	// #nosec G304 -- path is constructed inside downloadIntoCache from the
	// per-pool scratch directory; no user-controlled input flows here
	// unmediated.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open for magic check: %w", err)
	}
	defer func() { _ = f.Close() }()

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if magic != qcow2Magic {
		return fmt.Errorf("magic bytes %x are not QFI\\xfb", magic)
	}
	return nil
}

// DeleteImage removes {pool.root}/images/{basename} and its sidecar.
// Returns nil whether or not the files existed (idempotent — the CP-side
// delete handler relies on this to stay safe under agent-side manual
// cleanup or a previous ensure being unwound). Returns ErrPoolUnknown for
// unknown pools.
//
// Held under the per-(pool, basename) mutex to prevent a concurrent ensure
// race (the ensure goroutine renames into the same path).
func (m *Manager) DeleteImage(ctx context.Context, poolName, basename string) error {
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return ErrPoolUnknown
	}

	lock := m.lockForImage(poolName, basename)
	lock.Lock()
	defer lock.Unlock()

	imgPath := filepath.Join(p.root, imagesSubdir, basename)
	if err := os.Remove(imgPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", imgPath, err)
	}
	if err := os.Remove(imgPath + ".sha256"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove sidecar %s: %w", imgPath+".sha256", err)
	}
	return nil
}

// ListImages walks {pool.root}/images/ and returns the inventory of cached
// images: every regular file that is not a `.sha256` sidecar, paired with
// the sha read from its sidecar. Files lacking a well-formed sidecar are
// skipped — they could be a partial download or an operator's scratch file
// and should not surface as cached images. Returns ErrPoolUnknown for
// unknown pools, nil error and empty slice when images/ is absent.
//
// First-cut pagination: returns the entire inventory. ROADMAP entry
// "agent storage_images list — cursor pagination" tracks the deferred
// cursor implementation.
func (m *Manager) ListImages(ctx context.Context, poolName string) ([]CachedImage, error) {
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}

	imagesDir := filepath.Join(p.root, imagesSubdir)
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read images dir: %w", err)
	}

	images := make([]CachedImage, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sha256") {
			continue
		}
		imgPath := filepath.Join(imagesDir, name)
		sha, _, present := readCachedImage(imgPath, imgPath+".sha256")
		if !present {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		images = append(images, CachedImage{
			Basename:       name,
			ChecksumSHA256: sha,
			Format:         "qcow2",
			SizeBytes:      info.Size(),
			ImportedAt:     info.ModTime(),
		})
	}
	sort.Slice(images, func(i, j int) bool {
		return images[i].Basename < images[j].Basename
	})
	return images, nil
}

// isHexSHA256Lower returns true when s is a 64-char lowercase hex string.
// Mirrors agent.yaml's `pattern: "^[0-9a-f]{64}$"`.
func isHexSHA256Lower(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, ch := range s {
		isHex := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}
