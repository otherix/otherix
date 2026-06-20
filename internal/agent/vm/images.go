// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/storage"
)

// importHTTPTimeout caps the worst-case duration of a single image
// download. 1h is the agent-side default; operator-facing exposure of
// an import_timeout config is future work.
const importHTTPTimeout = time.Hour

// importMaxRedirects bounds redirect chain length for source URL
// fetches.
const importMaxRedirects = 5

// importDialTimeout bounds the TCP connect per dial/redirect hop.
const importDialTimeout = 30 * time.Second

// maxImageBytes caps the size of a single image download so a hostile or
// misconfigured source cannot fill the pool. 64 GiB is generous for any
// cloud image (Ubuntu Noble minimal cloudimg is under 300 MiB; even fat
// appliance images stay in the low tens of GiB). A var, not a const, solely
// so the over-cap test can lower it.
var maxImageBytes int64 = 64 << 30

// blockLocalDial rejects a dial to a node-local or special-use address:
// loopback (the agent's own services), link-local unicast (incl. the cloud
// IMDS at 169.254.169.254 and fe80::/10), multicast, and unspecified. That
// kills cloud-metadata and loopback/agent-local SSRF while intentionally
// ALLOWING RFC1918 + ULA private ranges - operators legitimately host VM
// image mirrors on private networks. It runs as the net.Dialer Control hook
// on the resolved IP of EVERY connection the image download makes -
// including redirect hops - so it is DNS-rebind-safe (SSRF guard).
func blockLocalDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse dial address %q: %v", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("image source resolved to non-IP address %q", host)
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("image source resolves to a node-local or special-use address %s", ip)
	}
	return nil
}

// imagesSubdir is the per-pool cache directory holding the image files and
// their sidecars (renamed from the former templates/ layout).
const imagesSubdir = "images"

// imageStoreLockPool is the synthetic pool dimension for the node-level image
// cache tier lock keys (the cache is node-level, not per-pool, so it shares no
// lock namespace with any real pool).
const imageStoreLockPool = "\x00image-store"

// sweepLegacyBasenameCache removes the regular files (cached image files and
// their .sha256 sidecars) left in a pool's legacy basename image cache directory
// imagesDir. PR #120 retired the write path into this cache - unpinned images now
// live in the node-level digest image store - so the directory holds only frozen
// pre-#120 entries that nothing reads as a cache-hit source. It removes only
// regular files in the immediate directory: it never recurses, never removes the
// directory itself (ensurePoolSubdirs keeps it; ListImages tolerates it empty),
// and is best-effort per file. Returns the count removed; an absent or unreadable
// directory yields 0.
func sweepLegacyBasenameCache(imagesDir string, log *slog.Logger) int {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if rmErr := os.Remove(filepath.Join(imagesDir, e.Name())); rmErr != nil {
			log.Warn("sweep legacy basename cache: remove",
				slog.String("file", e.Name()), slog.String("err", rmErr.Error()))
			continue
		}
		removed++
	}
	return removed
}

// Image-surface sentinel errors. Callers branch on errors.Is for envelope
// mapping; EnsureImage is synchronous so all of these surface directly to
// the create flow that drives it.
var (
	// ErrUnsupportedFormat is returned when the requested format is not
	// "qcow2". `raw` is a deferred enum extension.
	ErrUnsupportedFormat = errors.New("unsupported image format (qcow2 only)")
	// ErrMissingSourceURL is returned when the source URL is empty. Only
	// source_url is accepted; source_path is not yet implemented.
	ErrMissingSourceURL = errors.New("source_url is required")
	// ErrInvalidChecksumFormat is returned when a non-empty expected
	// checksum fails the 64-char lowercase hex pattern.
	ErrInvalidChecksumFormat = errors.New("expected_checksum_sha256 must be 64-char lowercase hex")
	// ErrInvalidImageBasename is returned when a derived cache identity is
	// not a safe single path segment (empty, "." / ".." traversal, or
	// containing a path separator). Such a value would let filepath.Join
	// collapse the cache path out of the images/ directory, so EnsureImage
	// rejects it before any filesystem action.
	ErrInvalidImageBasename = errors.New("image url basename is not a valid single path segment")
	// ErrCloneFailed wraps a failure to clone the cached image into the VM
	// disk path. EnsureImageInto returns it so the create flow can map the
	// failure to a clone_failed envelope distinct from image_unavailable.
	ErrCloneFailed = errors.New("clone image into disk path failed")
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
// stays alive in the map for the lifetime of the manager - cleanup of
// stale entries is deferred future work. The pool dimension is a string name (the
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

// validImageBasename reports whether name is a safe single-segment cache file
// identity: non-empty, not a "." / ".." traversal segment, and free of path
// separators. A traversal segment would let filepath.Join collapse the cache
// path out of the images/ directory (e.g. ".." resolves to the pool root), so
// the destructive DeleteImage path and the inline EnsureImage path both reject
// it before touching the filesystem.
func validImageBasename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsRune(name, '/') && !strings.ContainsRune(name, filepath.Separator)
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
//   - ErrInvalidImageBasename — the URL basename is not a safe single path
//     segment (empty, "." / ".." traversal, or contains a separator)
//   - *ChecksumMismatchError — verify mode and the served bytes disagree
//   - wrapped errors for download / qcow2-magic / filesystem failures
func (m *Manager) EnsureImage(ctx context.Context, poolName, sourceURL, expectedSHA, format string) (EnsureResult, error) {
	p, basename, err := m.resolveEnsure(poolName, sourceURL, expectedSHA, format)
	if err != nil {
		return EnsureResult{}, err
	}
	lock := m.lockForImage(poolName, basename)
	lock.Lock()
	defer lock.Unlock()
	return m.ensureLocked(ctx, p.root, basename, sourceURL, expectedSHA)
}

// EnsureImageInto materializes the image (download-on-miss, digest-verify) and
// clones it into dstPath, holding the per-image lock across both steps so a
// concurrent ensure for the same key cannot overwrite the cache file between the
// verify and the clone. When the node-level image store is wired, both pinned
// and unpinned images use it: pinned re-hashes the cache file bytes on a HIT
// (not the sidecar), so a tampered cache file routes to re-download; unpinned
// follows hint when present (best-effort, see ensureImageIntoUnpinned). Without
// the image store, the legacy per-pool basename cache is used. Clone failures
// are wrapped in ErrCloneFailed so the caller can map them to clone_failed.
func (m *Manager) EnsureImageInto(ctx context.Context, poolName, sourceURL, expectedSHA, format, hint, dstPath string) (EnsureResult, error) {
	p, basename, err := m.resolveEnsure(poolName, sourceURL, expectedSHA, format)
	if err != nil {
		return EnsureResult{}, err
	}
	// Pinned and unpinned both use the node-level content-addressed image cache
	// tier when it is available, so a peer can serve them and a second VM from
	// the same digest skips the download. Pinned keys by the supplied digest;
	// unpinned keys by the CP's resolved-digest hint when present, else by the
	// digest computed after download. The legacy per-pool basename cache is used
	// only when the image store is unwired (test paths / no artifact root).
	if m.imageStore != nil {
		if expectedSHA != "" {
			return m.ensureImageIntoPinned(ctx, sourceURL, expectedSHA, dstPath)
		}
		return m.ensureImageIntoUnpinned(ctx, sourceURL, basename, hint, dstPath)
	}
	lock := m.lockForImage(poolName, basename)
	lock.Lock()
	defer lock.Unlock()
	res, err := m.ensureLocked(ctx, p.root, basename, sourceURL, expectedSHA)
	if err != nil {
		return EnsureResult{}, err
	}
	if err := storage.CloneImage(res.Path, dstPath); err != nil {
		return EnsureResult{}, fmt.Errorf("%w: %v", ErrCloneFailed, err)
	}
	return res, nil
}

// ensureImageIntoPinned serves a pinned image from the node-level image cache
// tier. A hit clones from the cached blob with no network I/O; a miss downloads
// through the SSRF-guarded client, verifies the bytes hash to expectedSHA,
// validates the qcow2 header, stores the blob (content-addressed; Put re-verifies
// the digest), then clones. The per-digest lock serialises concurrent creates of
// the same pinned image so two goroutines do not download it twice.
func (m *Manager) ensureImageIntoPinned(ctx context.Context, sourceURL, expectedSHA, dstPath string) (EnsureResult, error) {
	lock := m.lockForImage(imageStoreLockPool, expectedSHA)
	lock.Lock()
	defer lock.Unlock()

	if m.imageStore.Has(expectedSHA) {
		blobPath, err := m.imageStore.BlobPath(expectedSHA)
		if err != nil {
			return EnsureResult{}, fmt.Errorf("resolve cached image path: %v", err)
		}
		fi, statErr := os.Stat(blobPath)
		if statErr != nil {
			return EnsureResult{}, fmt.Errorf("stat cached image: %v", statErr)
		}
		m.log.Info("image cache hit, cloning without download", slog.String("digest", expectedSHA))
		// Bump mtime so the eviction sweeper treats this as a recently-used image
		// (LRU by file mtime). Best-effort: a touch failure must not fail the create.
		now := time.Now()
		_ = os.Chtimes(blobPath, now, now)
		if err := storage.CloneImage(blobPath, dstPath); err != nil {
			return EnsureResult{}, fmt.Errorf("%w: %v", ErrCloneFailed, err)
		}
		return EnsureResult{SHA256: expectedSHA, Path: blobPath, SizeBytes: fi.Size()}, nil
	}

	scratch, err := os.MkdirTemp("", "otherix-image-*")
	if err != nil {
		return EnsureResult{}, fmt.Errorf("create scratch dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	tempPath := filepath.Join(scratch, "download")

	m.log.Info("image not cached, downloading from source", slog.String("digest", expectedSHA))
	_, computedSHA, dlErr := m.downloadAndHash(ctx, sourceURL, tempPath)
	if dlErr != nil {
		return EnsureResult{}, fmt.Errorf("download %s: %v", sourceURL, dlErr)
	}
	if computedSHA != expectedSHA {
		return EnsureResult{}, &ChecksumMismatchError{Expected: expectedSHA, Actual: computedSHA, URL: sourceURL}
	}
	if err := validateQcow2Magic(tempPath); err != nil {
		return EnsureResult{}, fmt.Errorf("qcow2_header_invalid: %v", err)
	}
	f, err := os.Open(tempPath) // #nosec G304 -- tempPath is the agent's own scratch dir.
	if err != nil {
		return EnsureResult{}, fmt.Errorf("open downloaded image: %v", err)
	}
	putErr := m.imageStore.Put(expectedSHA, f)
	_ = f.Close()
	if putErr != nil {
		return EnsureResult{}, fmt.Errorf("store image blob: %v", putErr)
	}
	// A new image landed; ask the eviction sweeper for an immediate pass so the
	// cache cannot drift far past its ceiling between ticks. Nil-safe.
	if m.imageEvictionNudge != nil {
		m.imageEvictionNudge()
	}
	blobPath, err := m.imageStore.BlobPath(expectedSHA)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("resolve stored image path: %v", err)
	}
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		return EnsureResult{}, fmt.Errorf("stat stored image: %v", statErr)
	}
	if err := storage.CloneImage(blobPath, dstPath); err != nil {
		return EnsureResult{}, fmt.Errorf("%w: %v", ErrCloneFailed, err)
	}
	return EnsureResult{SHA256: expectedSHA, Path: blobPath, SizeBytes: fi.Size()}, nil
}

// ensureImageIntoUnpinned materializes an unpinned image into dstPath through
// the node-level content-addressed image store. When hint is set and the store
// already holds it (e.g. the CP pre-pulled it from a peer), it clones from cache
// with no download. Otherwise it downloads sourceURL, computes the digest,
// stores the blob, writes the human-readable <digest>.meta sidecar, and clones.
// The hint is best-effort: a miss falls back to download, and the returned
// SHA256 is always the digest actually materialized (hint on a hit, computed on
// a download).
func (m *Manager) ensureImageIntoUnpinned(ctx context.Context, sourceURL, basename, hint, dstPath string) (EnsureResult, error) {
	if hint != "" {
		lock := m.lockForImage(imageStoreLockPool, hint)
		lock.Lock()
		hit := m.imageStore.Has(hint)
		if hit {
			res, err := m.cloneFromImageStore(hint, dstPath)
			lock.Unlock()
			if err != nil {
				return EnsureResult{}, err
			}
			m.writeImageMeta(hint, sourceURL, basename)
			return res, nil
		}
		lock.Unlock() // hint not present: fall through to download
	}

	// Coalesce concurrent unpinned imports of the same URL on this node into a
	// single download: the digest is unknown until the bytes arrive, so the
	// per-digest image lock cannot dedupe the fetch. singleflight keys by URL;
	// the leader downloads + stores, and every concurrent caller receives the
	// resulting digest and clones from cache. The shared call runs under the
	// leader's context (a singleflight property); a leader cancellation fails
	// the in-flight followers too, which is acceptable fail-open behaviour - a
	// retry starts a fresh download (singleflight drops the key when Do returns).
	v, doErr, _ := m.imageDownloadGroup.Do(sourceURL, func() (any, error) {
		return m.downloadStoreUnpinned(ctx, sourceURL)
	})
	if doErr != nil {
		return EnsureResult{}, doErr
	}
	computedSHA := v.(string)
	m.writeImageMeta(computedSHA, sourceURL, basename)
	// Clone under the per-digest lock so the eviction sweeper (nudged by the Put
	// above) or the scrubber cannot delete the freshly-stored blob between the Put
	// and CloneImage's open. A lost-race miss (the blob was evicted before we took
	// the lock) returns a retryable error so the create re-materializes it.
	lock := m.lockForImage(imageStoreLockPool, computedSHA)
	lock.Lock()
	defer lock.Unlock()
	if !m.imageStore.Has(computedSHA) {
		return EnsureResult{}, fmt.Errorf("image blob evicted before clone, retry: %s", computedSHA)
	}
	return m.cloneFromImageStore(computedSHA, dstPath)
}

// downloadStoreUnpinned downloads sourceURL into scratch, validates the qcow2
// header, stores the bytes in the node image store under their computed digest
// (Put re-verifies the digest), nudges the eviction sweeper, and returns the
// digest. It runs inside the per-URL singleflight (see ensureImageIntoUnpinned)
// so concurrent unpinned imports of the same URL share one download; the
// per-computed-digest lock still guards the Put against an unrelated pinned
// create of the same content.
func (m *Manager) downloadStoreUnpinned(ctx context.Context, sourceURL string) (string, error) {
	scratch, err := os.MkdirTemp("", "otherix-image-*")
	if err != nil {
		return "", fmt.Errorf("create scratch dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	tempPath := filepath.Join(scratch, "download")

	m.log.Info("unpinned image not cached, downloading from source", slog.String("url", sourceURL))
	_, computedSHA, dlErr := m.downloadAndHash(ctx, sourceURL, tempPath)
	if dlErr != nil {
		return "", fmt.Errorf("download %s: %v", sourceURL, dlErr)
	}
	if err := validateQcow2Magic(tempPath); err != nil {
		return "", fmt.Errorf("qcow2_header_invalid: %v", err)
	}

	lock := m.lockForImage(imageStoreLockPool, computedSHA)
	lock.Lock()
	defer lock.Unlock()

	f, err := os.Open(tempPath) // #nosec G304 -- agent's own scratch dir
	if err != nil {
		return "", fmt.Errorf("open downloaded image: %v", err)
	}
	putErr := m.imageStore.Put(computedSHA, f)
	_ = f.Close()
	if putErr != nil {
		return "", fmt.Errorf("store image blob: %v", putErr)
	}
	if m.imageEvictionNudge != nil {
		m.imageEvictionNudge()
	}
	return computedSHA, nil
}

// cloneFromImageStore resolves the cached blob path for digest, bumps its mtime
// (LRU for the eviction sweeper), and clones it into dstPath.
func (m *Manager) cloneFromImageStore(digest, dstPath string) (EnsureResult, error) {
	blobPath, err := m.imageStore.BlobPath(digest)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("resolve cached image path: %v", err)
	}
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		return EnsureResult{}, fmt.Errorf("stat cached image: %v", statErr)
	}
	now := time.Now()
	_ = os.Chtimes(blobPath, now, now)
	if err := storage.CloneImage(blobPath, dstPath); err != nil {
		return EnsureResult{}, fmt.Errorf("%w: %v", ErrCloneFailed, err)
	}
	return EnsureResult{SHA256: digest, Path: blobPath, SizeBytes: fi.Size()}, nil
}

// imageMetaDir returns the sibling directory holding <digest>.meta readability
// sidecars. It is intentionally separate from the image store's blob directory
// so the artifact-store janitor / eviction never treat a .meta as a stray blob
// or orphan sidecar.
func (m *Manager) imageMetaDir() string {
	return filepath.Join(filepath.Dir(m.imageStore.Root()), "images-meta")
}

// writeImageMeta writes a best-effort {url, basename} sidecar next-but-isolated
// to the cached blob so an operator can map a hex digest back to its source.
// Advisory only - never load-bearing - so any failure is logged and swallowed.
func (m *Manager) writeImageMeta(digest, sourceURL, basename string) {
	dir := m.imageMetaDir()
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // advisory metadata dir
		m.log.Warn("write image meta: mkdir", slog.String("digest", digest), slog.String("err", err.Error()))
		return
	}
	b, err := json.Marshal(struct {
		URL      string `json:"url"`
		Basename string `json:"basename"`
	}{URL: sourceURL, Basename: basename})
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, digest+".meta.tmp")
	final := filepath.Join(dir, digest+".meta")
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // advisory non-secret metadata
		m.log.Warn("write image meta: tmp", slog.String("digest", digest), slog.String("err", err.Error()))
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		m.log.Warn("write image meta: rename", slog.String("digest", digest), slog.String("err", err.Error()))
	}
}

// resolveEnsure runs the pre-flight validation (format, source URL, checksum
// shape, pool existence, basename safety) shared by EnsureImage and
// EnsureImageInto, returning the resolved pool and cache basename.
func (m *Manager) resolveEnsure(poolName, sourceURL, expectedSHA, format string) (pool, string, error) {
	if format != "qcow2" {
		return pool{}, "", fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	if sourceURL == "" {
		return pool{}, "", ErrMissingSourceURL
	}
	if expectedSHA != "" && !isHexSHA256Lower(expectedSHA) {
		return pool{}, "", ErrInvalidChecksumFormat
	}
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return pool{}, "", ErrPoolUnknown
	}
	basename := basenameFromURL(sourceURL)
	if !validImageBasename(basename) {
		return pool{}, "", ErrInvalidImageBasename
	}
	return p, basename, nil
}

// ensureLocked assumes the per-(pool, basename) lock is already held. It
// resolves the cache paths, serves a verified HIT, or downloads on a miss /
// stale slot. On a pinned-digest HIT it re-hashes the cache file bytes and
// compares to expectedSHA (not the sidecar): a disagreement falls through to
// the re-download path exactly like a sidecar disagreement. An unpinned HIT
// keeps name-keyed IfNotPresent reuse with no re-hash.
func (m *Manager) ensureLocked(ctx context.Context, root, basename, sourceURL, expectedSHA string) (EnsureResult, error) {
	imgPath := filepath.Join(root, imagesSubdir, basename)
	sidecarPath := imgPath + ".sha256"

	if cachedSHA, size, present := readCachedImage(imgPath, sidecarPath); present {
		switch {
		case expectedSHA == "":
			// Unpinned: name-keyed reuse, no re-hash.
			return EnsureResult{SHA256: cachedSHA, Path: imgPath, SizeBytes: size}, nil
		case cachedSHA == expectedSHA:
			// Pinned and the sidecar agrees: re-hash the file bytes to defend
			// against an out-of-band local write that left the sidecar stale.
			fileSHA, err := hashFile(imgPath)
			if err != nil {
				return EnsureResult{}, fmt.Errorf("re-hash cached image: %v", err)
			}
			if fileSHA == expectedSHA {
				return EnsureResult{SHA256: fileSHA, Path: imgPath, SizeBytes: size}, nil
			}
			// File bytes disagree with the pinned digest: stale slot -> re-download.
		default:
			// Sidecar disagrees with the pinned digest: stale slot -> re-download.
		}
	}
	return m.downloadIntoCache(ctx, root, basename, imgPath, sidecarPath, sourceURL, expectedSHA)
}

// hashFile computes the sha256 of the file at path as a lowercase hex string.
func hashFile(path string) (string, error) {
	// #nosec G304 -- path is an agent-owned cache path under a validated
	// basename ({pool.root}/images/{basename}), not caller-supplied input.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readCachedImage reports the cached sha (from the sidecar), the file size,
// and presence. A file present without a readable, well-formed sidecar is
// treated as absent so the next ensure recomputes both.
func readCachedImage(imgPath, sidecarPath string) (sha string, size int64, present bool) {
	info, err := os.Stat(imgPath)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", 0, false
	}
	// #nosec G304 -- sidecarPath is {pool.root}/images/{basename}.sha256, an
	// agent-owned path under a validated basename, not caller-supplied input.
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

	size, computedSHA, dlErr := m.downloadAndHash(ctx, sourceURL, tempPath)
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
//
// Every dial - including each redirect hop - goes through m.imageDialControl
// (blockLocalDial in production), which vets the resolved IP post-DNS.
// The body is read through a maxImageBytes cap that fails closed: an
// over-cap source errors out rather than truncating into the cache.
func (m *Manager) downloadAndHash(ctx context.Context, sourceURL, tempPath string) (int64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, importHTTPTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("build request: %w", err)}
	}

	// Clone the default transport so its defaults (TLSHandshakeTimeout,
	// ForceAttemptHTTP2, idle-conn pooling, ...) carry over, then override
	// only what the SSRF posture requires. A forward proxy would terminate
	// the connection at the proxy and route onward from there, so the dial
	// Control hook would only ever see the proxy's IP - blinding the SSRF
	// guard. Disable proxying for image downloads so the guard always
	// inspects the real image-host IP.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: importDialTimeout, Control: m.imageDialControl}).DialContext
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= importMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}
	defer client.CloseIdleConnections()
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
	// Read one byte past the cap so an exactly-at-cap image passes while an
	// over-cap source is detected and rejected (never silently truncated).
	size, copyErr := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(resp.Body, maxImageBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("stream body: %w", copyErr)}
	}
	if closeErr != nil {
		return 0, "", &downloadError{cause: fmt.Errorf("close temp file: %w", closeErr)}
	}
	if size > maxImageBytes {
		return 0, "", &downloadError{cause: fmt.Errorf("image exceeds max size %d bytes", maxImageBytes)}
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// qcow2Magic is the first 4 bytes of every valid qcow2 image: 'QFI\xfb'
// (0x514649fb big-endian). Magic-only validation; full 9-rule header
// validation (the full qcow2 header check) is deferred future work.
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
// race (the ensure goroutine renames into the same path). A basename that is
// not a safe single path segment (empty, "." / ".." traversal, or containing a
// separator) is rejected with ErrInvalidChecksumFormat before any filesystem
// call, so a crafted delete key can never collapse the path onto the pool root.
func (m *Manager) DeleteImage(ctx context.Context, poolName, basename string) error {
	if !validImageBasename(basename) {
		return ErrInvalidChecksumFormat
	}
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
// First-cut pagination: returns the entire inventory. The agent
// image-cache list - cursor pagination tracks the deferred
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
