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
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// importHTTPTimeout caps the worst-case duration of a single import
// download. 1h is the agent-side default; ROADMAP entry
// "agent.storage.dir.import_timeout config" tracks operator-facing
// exposure.
const importHTTPTimeout = time.Hour

// importMaxRedirects bounds redirect chain length for source URL
// fetches.
const importMaxRedirects = 5

// Image-surface sentinel errors. Handlers map to 400 / 404 envelopes;
// task-error envelopes for async failures live in TaskError on the
// agent task itself.
var (
	// ErrUnsupportedFormat is returned when ImportImageRequest.Format is
	// not "qcow2". `raw` is a deferred enum extension.
	ErrUnsupportedFormat = errors.New("unsupported image format (qcow2 only)")
	// ErrMissingSourceURL is returned when ImportImageRequest.SourceURL
	// is empty. Only source_url is accepted; source_path not implemented
	// (separate iteration tracked in ROADMAP).
	ErrMissingSourceURL = errors.New("source_url is required")
	// ErrInvalidChecksumFormat is returned when expected_checksum_sha256
	// fails the 64-char lowercase hex pattern.
	ErrInvalidChecksumFormat = errors.New("expected_checksum_sha256 must be 64-char lowercase hex")
)

// ImportImageRequest is the agent-internal struct the HTTP handler
// constructs from the wire body. Wire shape mirrors agent.yaml's
// ImageImportRequest; this struct stays minimal by design — only
// source_url is accepted, format must be "qcow2".
type ImportImageRequest struct {
	SourceURL              string
	ExpectedChecksumSHA256 string
	Format                 string
}

// CachedImage is the agent-internal view of an entry in the pool's
// image cache, projected by ListImages from a filesystem walk of
// {pool.Root}/templates/. The HTTP handler shapes this to the wire
// CachedImage schema.
type CachedImage struct {
	ChecksumSHA256 string
	Format         string
	Path           string
	SizeBytes      int64
	LastUsedAt     *time.Time
	SourceURL      *string
}

// imageLockKey indexes the per-(pool, sha256) mutex preventing
// concurrent imports of the same content from clobbering each other.
// The mutex stays alive in the map for the lifetime of the manager —
// cleanup is a future iteration concern. The pool dimension is a
// string name (the agent's pool registry is name-keyed).
type imageLockKey struct {
	pool string
	sha  string
}

// lockForImage returns the mutex for (poolName, sha256). LoadOrStore
// keeps a stable identity, so two concurrent goroutines see the same
// *sync.Mutex and serialise correctly.
func (m *Manager) lockForImage(poolName, sha string) *sync.Mutex {
	actual, _ := m.imageLocks.LoadOrStore(imageLockKey{pool: poolName, sha: sha}, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	return mu
}

// ImportImage downloads the image identified by req.SourceURL,
// optionally verifies its sha256, validates qcow2 magic, and atomically
// places it at {pool.Root}/templates/{sha}.{format}. Returns 202 + a
// pending task immediately; the goroutine progresses to
// running → success/failed.
//
// Two modes:
//
//   - **Verify mode** (req.ExpectedChecksumSHA256 non-empty): pre-
//     existence check stats {pool.Root}/templates/{expected}.qcow2 and
//     returns terminal-success without re-download when the file is
//     already present. Otherwise the streamed bytes are compared
//     against expected after download; mismatch terminates with
//     `checksum_mismatch`.
//   - **Compute mode** (req.ExpectedChecksumSHA256 empty): no pre-
//     existence fast-path (filename unknown until download completes);
//     the download streams to a task-scoped scratch path, the agent
//     computes the sha256 in-flight, and the atomic rename lands the
//     file at {pool.Root}/templates/{computed}.qcow2. The computed
//     checksum is returned in the task result so the CP-side worker
//     can back-propagate it onto the template row
//     (UpdateTemplateImageChecksumIfNull).
//
// The agent always returns the computed checksum in the terminal-
// success result regardless of mode; verify mode just adds a gate.
//
// Errors (synchronous, caller-facing):
//   - ErrPoolUnknown — pool_id is not configured
//   - ErrUnsupportedFormat — format != "qcow2"
//   - ErrMissingSourceURL — source_url empty
//   - ErrInvalidChecksumFormat — non-empty checksum is not 64-char
//     lowercase hex (empty checksum is allowed: it signals compute mode)
//
// Errors (async, task.error envelope):
//   - download_failed — HTTP error, network failure, non-200 source
//   - checksum_mismatch — computed sha != expected (verify mode only)
//   - qcow2_header_invalid — magic bytes do not match QFI\xfb
//   - internal — filesystem failure (mkdir/rename/etc.)
func (m *Manager) ImportImage(ctx context.Context, poolName string, req ImportImageRequest) (*AgentTask, error) {
	if req.Format != "qcow2" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedFormat, req.Format)
	}
	if req.SourceURL == "" {
		return nil, ErrMissingSourceURL
	}
	// Accept empty checksum (compute mode); validate the
	// 64-char-lowercase-hex pattern only when the operator supplied
	// a value.
	if req.ExpectedChecksumSHA256 != "" && !isHexSHA256Lower(req.ExpectedChecksumSHA256) {
		return nil, ErrInvalidChecksumFormat
	}

	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}

	// Per-task correlation id; pool dimension carried explicitly through
	// runImageImport's poolName arg.
	task := m.tasks.Create(TaskKindStorageImageImport, uuid.New())

	// #nosec G118 -- async task work intentionally outlives the HTTP
	// request; CP polls /v1/tasks/{id} to observe progress.
	go m.runImageImport(task.ID, poolName, p.root, req)
	return task, nil
}

// runImageImport executes the download/verify/place pipeline inside
// the per-(pool, sha) mutex. Simplified scope: magic-only qcow2
// validation, no chattr +i; compute-mode adds streaming sha
// computation without verification.
//
// Lock scope: keyed on (poolID, ExpectedChecksumSHA256). Verify-mode
// imports share the lock for the same target checksum so concurrent
// callers serialise correctly. Compute-mode imports key on the empty
// string — distinct compute-mode imports against the same pool
// serialise via the per-pool empty-string lock. The cost is mild
// (one compute-mode import at a time per pool); the alternative
// (locking only on output path) would require a post-download lock
// re-acquire that does not buy much in practice.
func (m *Manager) runImageImport(taskID uuid.UUID, poolName, poolRoot string, req ImportImageRequest) {
	verifyMode := req.ExpectedChecksumSHA256 != ""
	log := m.log.With(
		"task_id", taskID.String(),
		"pool_name", poolName,
		"expected_checksum", req.ExpectedChecksumSHA256,
		"mode", modeLabel(verifyMode),
		"source_url", req.SourceURL,
	)
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	lock := m.lockForImage(poolName, req.ExpectedChecksumSHA256)
	lock.Lock()
	defer lock.Unlock()

	// Pre-existence fast-path applies to verify mode only. Compute
	// mode cannot stat a filename it does not yet know — the agent
	// always streams, then the back-propagation flow on the CP side
	// ensures a repeat materialisation hits this branch (the template
	// row's checksum is filled after the first compute-mode success).
	if verifyMode {
		finalPath := filepath.Join(poolRoot, "templates", req.ExpectedChecksumSHA256+"."+req.Format)
		if info, err := os.Stat(finalPath); err == nil && !info.IsDir() && info.Size() > 0 {
			m.finishImportSuccess(taskID, req.ExpectedChecksumSHA256, info.Size(), req.Format)
			log.Info("image already present; idempotent success", "size_bytes", info.Size())
			return
		}
	}

	scratchDir := filepath.Join(poolRoot, "scratch", "import", taskID.String())
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		log.Error("mkdir scratch", "err", err)
		m.failImportTask(taskID, "internal", fmt.Sprintf("create scratch dir: %v", err), nil)
		return
	}
	defer func() {
		if err := os.RemoveAll(scratchDir); err != nil {
			log.Warn("cleanup scratch dir", "err", err)
		}
	}()

	tempPath := filepath.Join(scratchDir, "image."+req.Format)
	size, computedSHA, dlErr := downloadAndHash(req.SourceURL, tempPath)
	if dlErr != nil {
		var de *downloadError
		switch {
		case errors.As(dlErr, &de):
			m.failImportTask(taskID, "download_failed", de.Error(),
				map[string]any{"source_url": req.SourceURL, "status": de.status})
		default:
			m.failImportTask(taskID, "download_failed", dlErr.Error(),
				map[string]any{"source_url": req.SourceURL})
		}
		log.Warn("download failed", "err", dlErr)
		return
	}

	// Verify-mode gate: only fail-fast when the operator supplied
	// expected_checksum_sha256. Compute mode trusts the source URL
	// integrity (TLS + reputable mirror).
	if verifyMode && computedSHA != req.ExpectedChecksumSHA256 {
		log.Warn("checksum mismatch", "actual", computedSHA)
		m.failImportTask(taskID, "checksum_mismatch",
			"downloaded content sha256 does not match expected_checksum_sha256",
			map[string]any{"expected": req.ExpectedChecksumSHA256, "actual": computedSHA})
		return
	}

	if err := validateQcow2Magic(tempPath); err != nil {
		log.Warn("qcow2 magic invalid", "err", err)
		m.failImportTask(taskID, "qcow2_header_invalid", err.Error(),
			map[string]any{"reason": "invalid_magic"})
		return
	}

	// Storage layout invariant: the on-disk filename always equals
	// the computed (or, equivalently in verify mode, expected) sha.
	// Compute mode determines the filename here, after the download
	// completes; verify mode would land at the same path because
	// computedSHA == expectedSHA on success.
	finalPath := filepath.Join(poolRoot, "templates", computedSHA+"."+req.Format)
	if err := os.Rename(tempPath, finalPath); err != nil {
		log.Error("atomic rename", "err", err)
		m.failImportTask(taskID, "internal", fmt.Sprintf("rename to templates: %v", err), nil)
		return
	}

	m.finishImportSuccess(taskID, computedSHA, size, req.Format)
	log.Info("image imported", "size_bytes", size, "final_path", finalPath, "computed_checksum", computedSHA)
}

// modeLabel renders the mode label for slog. Kept a tiny helper for
// log-tag consistency across runImageImport call sites.
func modeLabel(verify bool) string {
	if verify {
		return "verify"
	}
	return "compute"
}

// downloadError carries the source-side HTTP status alongside the
// underlying network error so failImportTask can populate
// details.status. status==0 means transport-level failure (DNS, TLS,
// connection reset, …) before any response status was observed.
type downloadError struct {
	status int
	cause  error
}

func (d *downloadError) Error() string { return d.cause.Error() }
func (d *downloadError) Unwrap() error { return d.cause }

// downloadAndHash streams sourceURL → tempPath while hashing the body
// in one pass. Returns (size_bytes, hex_sha256, nil) on success or
// (0, "", *downloadError) on any failure. HTTP-level failures populate
// downloadError.status; transport failures leave it zero.
func downloadAndHash(sourceURL, tempPath string) (int64, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), importHTTPTimeout)
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

	// #nosec G304 -- tempPath is constructed inside ImportImage from
	// poolRoot (operator-configured) + task uuid (server-minted) — no
	// user-controlled component reaches this path.
	// Mirrors internal/agent/storage/clone.go's 0o600 permission for
	// CP-written image files; the agent process owns the pool root and
	// is the only writer.
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

// validateQcow2Magic reads the first 4 bytes of path and rejects
// anything other than qcow2Magic. Magic-only validation; the remaining
// 8 header rules (version, backing_file, crypt_method, cluster_bits,
// virtual_size, v3 extensions) ride on the deferred work item.
func validateQcow2Magic(path string) error {
	// #nosec G304 -- path is constructed inside runImageImport from
	// the per-task scratch directory; no user-controlled input flows
	// here unmediated.
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

// finishImportSuccess writes the terminal-success projection. Result
// fields (`checksum_sha256`, `size_bytes`, `format`) match the
// CP-side agent_import_executor.decodeImportResult contract.
func (m *Manager) finishImportSuccess(taskID uuid.UUID, sha string, size int64, format string) {
	result, _ := json.Marshal(map[string]any{
		"checksum_sha256": sha,
		"size_bytes":      size,
		"format":          format,
	})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
}

// failImportTask marks the task failed with the supplied envelope. Code
// is the snake_case identifier mapped to response.ErrorCode by CP-side
// classifyImportError; message and details are operator-facing.
func (m *Manager) failImportTask(taskID uuid.UUID, code, message string, details map[string]any) {
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{
			Code:    code,
			Message: message,
			Details: details,
		}
	})
}

// DeleteImage removes {pool.Root}/templates/{checksum}.qcow2. Returns
// nil whether or not the file existed (idempotent — the CP-side
// delete handler relies on this to stay safe under agent-side manual
// cleanup or a previous import being unwound). Returns
// ErrPoolUnknown for unknown pools.
//
// Held under the per-(pool, sha) mutex to prevent a concurrent import
// race (import goroutine renames into the same path).
func (m *Manager) DeleteImage(ctx context.Context, poolName, checksum string) error {
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return ErrPoolUnknown
	}
	if !isHexSHA256Lower(checksum) {
		return ErrInvalidChecksumFormat
	}

	lock := m.lockForImage(poolName, checksum)
	lock.Lock()
	defer lock.Unlock()

	path := filepath.Join(p.root, "templates", checksum+".qcow2")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// ListImages walks {pool.Root}/templates/ and returns the inventory of
// qcow2 files matching {64-char lowercase hex sha}.qcow2. Entries
// failing the naming convention are silently skipped — they could be
// the operator's manual scratch files and shouldn't surface as cached
// images. Returns ErrPoolUnknown for unknown pools, nil error and
// empty slice when templates/ is absent.
//
// First-cut pagination: returns the entire inventory. ROADMAP entry
// "agent storage_images list — cursor pagination" tracks the
// deferred cursor implementation.
func (m *Manager) ListImages(ctx context.Context, poolName string) ([]CachedImage, error) {
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}

	templatesDir := filepath.Join(p.root, "templates")
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read templates dir: %w", err)
	}

	images := make([]CachedImage, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := filepath.Ext(name)
		if ext != ".qcow2" {
			continue
		}
		sha := name[:len(name)-len(ext)]
		if !isHexSHA256Lower(sha) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mtime := info.ModTime()
		images = append(images, CachedImage{
			ChecksumSHA256: sha,
			Format:         "qcow2",
			Path:           filepath.Join(templatesDir, name),
			SizeBytes:      info.Size(),
			LastUsedAt:     &mtime,
		})
	}
	sort.Slice(images, func(i, j int) bool {
		return images[i].ChecksumSHA256 < images[j].ChecksumSHA256
	})
	return images, nil
}

// isHexSHA256Lower returns true when s is a 64-char lowercase hex
// string. Mirrors agent.yaml's `pattern: "^[0-9a-f]{64}$"`.
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
