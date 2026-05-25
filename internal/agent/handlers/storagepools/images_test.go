// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// validQcow2 returns the canonical happy-path body — magic-prefixed
// blob deterministic for sha256.
func validQcow2() []byte {
	b := make([]byte, 64)
	b[0] = 'Q'
	b[1] = 'F'
	b[2] = 'I'
	b[3] = 0xfb
	return b
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// poolRootFromManager extracts the pool root used by newTestManager.
// The helper is colocated with the manager fixture, but the tests
// here need filesystem assertions; mirror the layout convention used
// in newTestManager — pool.Root = filepath.Join(tmp, "pools",
// "default") — by accepting the manager's t.TempDir lineage. Simpler
// to recreate the test-environment plumbing here so the handler tests
// stay self-contained.
//
// We re-implement newTestManager inline to control the pool root path.

func TestImportImage_HappyPath(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	body := validQcow2()
	sha := hexSum(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	reqBody, _ := json.Marshal(map[string]any{
		"source_url":               srv.URL,
		"expected_checksum_sha256": sha,
		"format":                   "qcow2",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/"+poolID+"/images",
		bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", rec.Code, rec.Body.String())
	}
	var accepted asyncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	taskID, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Errorf("task_id = %q, want UUID", accepted.TaskID)
	}
	switch accepted.Status {
	case "pending", "running":
		// expected
	default:
		t.Errorf("status = %q, want pending or running", accepted.Status)
	}
	drainAgentTask(t, m, taskID, 5*time.Second)
}

func TestImportImage_UnknownPool(t *testing.T) {
	m, _ := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"source_url":               "http://example.invalid",
		"expected_checksum_sha256": hexSum([]byte("x")),
		"format":                   "qcow2",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/"+uuid.New().String()+"/images",
		bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// TestImportImage_NonUUIDPoolName confirms that any non-empty string
// in the URL is treated as a pool name. Unknown names — including
// strings that don't parse as UUIDs — return 404 not_found from the
// manager, not 400 validation_failed.
func TestImportImage_NonUUIDPoolName(t *testing.T) {
	m, _ := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"source_url":               "http://example.invalid",
		"expected_checksum_sha256": hexSum([]byte("x")),
		"format":                   "qcow2",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/not-a-uuid/images", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestImportImage_BadJSON(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/"+poolID+"/images",
		strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed code; got %s", rec.Body.String())
	}
}

func TestImportImage_SourcePathRejected(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"source_path":              "/etc/passwd",
		"expected_checksum_sha256": hexSum([]byte("x")),
		"format":                   "qcow2",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/"+poolID+"/images",
		bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported_format") {
		t.Errorf("body missing unsupported_format code; got %s", rec.Body.String())
	}
}

func TestImportImage_MissingSourceURL(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"expected_checksum_sha256": hexSum([]byte("x")),
		"format":                   "qcow2",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/"+poolID+"/images",
		bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestImportImage_UnsupportedFormat(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"source_url":               "http://example.invalid",
		"expected_checksum_sha256": hexSum([]byte("x")),
		"format":                   "raw",
	})
	req := httptest.NewRequest(http.MethodPost,
		"/v1/storage-pools/"+poolID+"/images",
		bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported_format") {
		t.Errorf("body missing unsupported_format code; got %s", rec.Body.String())
	}
}

func TestDeleteImage_HappyPath(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	// Seed a file at templates/{sha}.qcow2.
	poolRoot := poolRootForTest(t, m, poolID)
	body := validQcow2()
	sha := hexSum(body)
	if err := os.WriteFile(filepath.Join(poolRoot, "templates", sha+".qcow2"), body, 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/v1/storage-pools/"+poolID+"/images/"+sha, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(poolRoot, "templates", sha+".qcow2")); !os.IsNotExist(err) {
		t.Errorf("file survived; err = %v", err)
	}
}

func TestDeleteImage_Idempotent(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	sha := hexSum([]byte("missing"))
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/storage-pools/"+poolID+"/images/"+sha, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (idempotent); body = %s",
			rec.Code, rec.Body.String())
	}
}

func TestDeleteImage_UnknownPool(t *testing.T) {
	m, _ := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	sha := hexSum([]byte("x"))
	req := httptest.NewRequest(http.MethodDelete,
		"/v1/storage-pools/"+uuid.New().String()+"/images/"+sha, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteImage_MalformedChecksum(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodDelete,
		"/v1/storage-pools/"+poolID+"/images/not-a-sha", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestListImages_Empty(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/storage-pools/"+poolID+"/images", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var list cachedImageList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if len(list.Data) != 0 {
		t.Errorf("len(data) = %d, want 0", len(list.Data))
	}
	if list.Meta.NextCursor != nil {
		t.Errorf("meta.next_cursor = %v, want nil", *list.Meta.NextCursor)
	}
}

func TestListImages_Populated(t *testing.T) {
	m, poolID := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	poolRoot := poolRootForTest(t, m, poolID)
	body := validQcow2()
	sha := hexSum(body)
	if err := os.WriteFile(filepath.Join(poolRoot, "templates", sha+".qcow2"), body, 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/v1/storage-pools/"+poolID+"/images", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var list cachedImageList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if len(list.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1; got = %+v", len(list.Data), list.Data)
	}
	got := list.Data[0]
	if got.ChecksumSHA256 != sha {
		t.Errorf("checksum = %q, want %q", got.ChecksumSHA256, sha)
	}
	if got.Format != "qcow2" {
		t.Errorf("format = %q, want qcow2", got.Format)
	}
	if got.SizeBytes != int64(len(body)) {
		t.Errorf("size_bytes = %d, want %d", got.SizeBytes, len(body))
	}
	if got.LastUsedAt == nil {
		t.Errorf("last_used_at = nil, want timestamp")
	}
	if got.SourceURL != nil {
		t.Errorf("source_url = %v, want nil", *got.SourceURL)
	}
}

func TestListImages_UnknownPool(t *testing.T) {
	m, _ := newTestManager(t)
	h := New(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/storage-pools/"+uuid.New().String()+"/images", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}
