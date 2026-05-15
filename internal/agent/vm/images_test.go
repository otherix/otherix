// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// validQcow2Bytes returns а synthetic blob whose first 4 bytes match
// the qcow2 magic 'QFI\xfb'. The remaining payload is arbitrary —
// magic-only validation (Q2 scope decision) ignores everything past
// byte 4. Each byte after the magic adds к the sha256 deterministically,
// so tests can pre-compute the expected hex digest.
func validQcow2Bytes() []byte {
	b := make([]byte, 64)
	b[0] = 'Q'
	b[1] = 'F'
	b[2] = 'I'
	b[3] = 0xfb
	// bytes 4..63 stay zero; deterministic для sha256 verification.
	return b
}

// invalidMagicBytes returns а blob whose first 4 bytes are NOT the
// qcow2 magic. Used to drive the qcow2_header_invalid task-error path.
func invalidMagicBytes() []byte {
	return []byte{'N', 'O', 'P', 'E', 0, 0, 0, 0}
}

// hexSHA256 returns the hex sha256 of b.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// serveBytes spins up an httptest server that responds к GET requests
// с the supplied payload (HTTP 200 + body). Returns the URL и а
// cleanup func.
func serveBytes(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// serveBytesCounting mirrors serveBytes but accumulates the cumulative
// bytes served into *served. Used by compute-mode pre-existence tests
// to prove the download path actually engaged (compute mode must not
// short-circuit on а pre-staged file because the filename is unknown
// upfront).
func serveBytesCounting(t *testing.T, body []byte, served *int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n, _ := w.Write(body)
		atomic.AddInt64(served, int64(n))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// serveStatus spins up an httptest server that responds к every GET
// с the supplied status (no body).
func serveStatus(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestManager_ImportImage_UnknownPool(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = m.ImportImage(t.Context(), "unknown-pool", ImportImageRequest{
		SourceURL:              "http://example.invalid",
		ExpectedChecksumSHA256: hexSHA256([]byte("x")),
		Format:                 "qcow2",
	})
	if !errors.Is(err, ErrPoolUnknown) {
		t.Fatalf("err = %v, want ErrPoolUnknown", err)
	}
}

func TestManager_ImportImage_ValidationErrors(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	validSha := hexSHA256([]byte("x"))
	cases := []struct {
		name string
		req  ImportImageRequest
		want error
	}{
		{
			name: "unsupported format",
			req:  ImportImageRequest{SourceURL: "http://x.invalid", ExpectedChecksumSHA256: validSha, Format: "raw"},
			want: ErrUnsupportedFormat,
		},
		{
			name: "missing source url",
			req:  ImportImageRequest{SourceURL: "", ExpectedChecksumSHA256: validSha, Format: "qcow2"},
			want: ErrMissingSourceURL,
		},
		{
			// Empty checksum is compute mode, not an error. The
			// validator must NOT reject - only malformed non-empty
			// values fail. The test asserts а non-error outcome here;
			// the surrounding test loop expects а sentinel for every
			// row, so this case lives separately below in а dedicated
			// subtest.
			name: "checksum too short",
			req:  ImportImageRequest{SourceURL: "http://x.invalid", ExpectedChecksumSHA256: "deadbeef", Format: "qcow2"},
			want: ErrInvalidChecksumFormat,
		},
		{
			name: "checksum uppercase",
			req: ImportImageRequest{
				SourceURL:              "http://x.invalid",
				ExpectedChecksumSHA256: "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD",
				Format:                 "qcow2",
			},
			want: ErrInvalidChecksumFormat,
		},
		{
			name: "checksum non-hex",
			req: ImportImageRequest{
				SourceURL:              "http://x.invalid",
				ExpectedChecksumSHA256: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
				Format:                 "qcow2",
			},
			want: ErrInvalidChecksumFormat,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.ImportImage(t.Context(), poolName, tc.req)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// An empty expected_checksum_sha256 signals compute mode and must
	// NOT trigger ErrInvalidChecksumFormat. The validator's gate kicks
	// in only for non-empty, non-canonical values.
	t.Run("empty checksum accepted as compute mode", func(t *testing.T) {
		url := serveBytes(t, validQcow2Bytes())
		task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
			SourceURL:              url,
			ExpectedChecksumSHA256: "",
			Format:                 "qcow2",
		})
		if err != nil {
			t.Fatalf("compute-mode validation rejected empty checksum: %v", err)
		}
		// Drain the goroutine so it does not race the next subtest's
		// pool cleanup.
		_ = waitForTaskTerminal(t, m, task.ID, 5*time.Second)
	})
}

func TestManager_ImportImage_HappyPath(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body := validQcow2Bytes()
	expectedSha := hexSHA256(body)
	url := serveBytes(t, body)

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: expectedSha,
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}
	if task.Kind != TaskKindStorageImageImport {
		t.Errorf("task.Kind = %q, want %q", task.Kind, TaskKindStorageImageImport)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 5*time.Second)
	if terminal.Status != TaskStatusSuccess {
		t.Fatalf("terminal.Status = %q, want %q; error = %+v",
			terminal.Status, TaskStatusSuccess, terminal.Error)
	}

	var result struct {
		ChecksumSHA256 string `json:"checksum_sha256"`
		SizeBytes      int64  `json:"size_bytes"`
		Format         string `json:"format"`
	}
	if err := json.Unmarshal(terminal.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ChecksumSHA256 != expectedSha {
		t.Errorf("result.checksum_sha256 = %q, want %q", result.ChecksumSHA256, expectedSha)
	}
	if result.SizeBytes != int64(len(body)) {
		t.Errorf("result.size_bytes = %d, want %d", result.SizeBytes, len(body))
	}
	if result.Format != "qcow2" {
		t.Errorf("result.format = %q, want qcow2", result.Format)
	}

	// File materialised at templates/{sha}.qcow2.
	finalPath := filepath.Join(poolRoot, "templates", expectedSha+".qcow2")
	info, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("stat final path: %v", err)
	}
	if info.Size() != int64(len(body)) {
		t.Errorf("final file size = %d, want %d", info.Size(), len(body))
	}

	// Scratch swept.
	scratchDir := filepath.Join(poolRoot, "scratch", "import", task.ID.String())
	if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir survived; err = %v", err)
	}
}

func TestManager_ImportImage_PreExistence(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body := validQcow2Bytes()
	expectedSha := hexSHA256(body)
	finalPath := filepath.Join(poolRoot, "templates", expectedSha+".qcow2")
	if err := os.WriteFile(finalPath, body, 0o640); err != nil {
		t.Fatalf("seed final path: %v", err)
	}

	// Server should never be called, but provide а valid URL to keep
	// the validation surface honest.
	url := serveBytes(t, []byte("should not be downloaded"))

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: expectedSha,
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 2*time.Second)
	if terminal.Status != TaskStatusSuccess {
		t.Fatalf("terminal.Status = %q, want %q; error = %+v",
			terminal.Status, TaskStatusSuccess, terminal.Error)
	}

	// File content unchanged (would have been overwritten if download
	// path engaged).
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final path: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("final content mutated; idempotent pre-existence path должен сохранить existing file")
	}
}

// TestManager_ImportImage_ComputeMode_HappyPath locks in compute
// mode: the request omits expected_checksum_sha256, the agent
// downloads, computes the sha in-flight, и atomically renames к
// templates/{computed}.qcow2. The terminal result carries the
// computed checksum so the CP-side worker can back-propagate it.
func TestManager_ImportImage_ComputeMode_HappyPath(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body := validQcow2Bytes()
	wantSha := hexSHA256(body)
	url := serveBytes(t, body)

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: "", // compute mode: empty signals "no upstream hash"
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 5*time.Second)
	if terminal.Status != TaskStatusSuccess {
		t.Fatalf("terminal.Status = %q, want %q; error = %+v",
			terminal.Status, TaskStatusSuccess, terminal.Error)
	}

	var result struct {
		ChecksumSHA256 string `json:"checksum_sha256"`
		SizeBytes      int64  `json:"size_bytes"`
		Format         string `json:"format"`
	}
	if err := json.Unmarshal(terminal.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ChecksumSHA256 != wantSha {
		t.Errorf("result.checksum_sha256 = %q, want %q (computed sha)",
			result.ChecksumSHA256, wantSha)
	}

	// Storage layout invariant: file landed at the computed-checksum path.
	finalPath := filepath.Join(poolRoot, "templates", wantSha+".qcow2")
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("stat final path: %v", err)
	}
}

// TestManager_ImportImage_ComputeMode_NoPreExistenceFastPath confirms
// compute mode never short-circuits на а pre-existing file. The
// filename is unknown upfront, so the agent always streams; CP-side
// back-propagation turns the template into verify mode for next time.
func TestManager_ImportImage_ComputeMode_NoPreExistenceFastPath(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body := validQcow2Bytes()
	expectedSha := hexSHA256(body)
	finalPath := filepath.Join(poolRoot, "templates", expectedSha+".qcow2")
	// Pre-seed the file. Verify mode would skip the download, compute
	// mode must NOT — it has no checksum к key the stat against.
	if err := os.WriteFile(finalPath, body, 0o640); err != nil {
		t.Fatalf("seed final path: %v", err)
	}

	// Server counts how many bytes were served; non-zero proves the
	// download path engaged.
	served := int64(0)
	url := serveBytesCounting(t, body, &served)

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: "",
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 5*time.Second)
	if terminal.Status != TaskStatusSuccess {
		t.Fatalf("terminal.Status = %q, want %q; error = %+v",
			terminal.Status, TaskStatusSuccess, terminal.Error)
	}
	if served == 0 {
		t.Errorf("download path was skipped; compute mode must always stream")
	}
}

func TestManager_ImportImage_ChecksumMismatch(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body := validQcow2Bytes()
	// Hand the request а sha that doesn't match the body's actual sha.
	wrongSha := hexSHA256([]byte("different"))
	url := serveBytes(t, body)

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: wrongSha,
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 2*time.Second)
	if terminal.Status != TaskStatusFailed {
		t.Fatalf("terminal.Status = %q, want %q", terminal.Status, TaskStatusFailed)
	}
	if terminal.Error == nil || terminal.Error.Code != "checksum_mismatch" {
		t.Fatalf("error = %+v, want code=checksum_mismatch", terminal.Error)
	}
	if got := terminal.Error.Details["expected"]; got != wrongSha {
		t.Errorf("details.expected = %v, want %q", got, wrongSha)
	}
	if got := terminal.Error.Details["actual"]; got != hexSHA256(body) {
		t.Errorf("details.actual = %v, want %q", got, hexSHA256(body))
	}
}

func TestManager_ImportImage_Qcow2HeaderInvalid(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body := invalidMagicBytes()
	expectedSha := hexSHA256(body)
	url := serveBytes(t, body)

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: expectedSha,
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 2*time.Second)
	if terminal.Status != TaskStatusFailed {
		t.Fatalf("terminal.Status = %q, want %q", terminal.Status, TaskStatusFailed)
	}
	if terminal.Error == nil || terminal.Error.Code != "qcow2_header_invalid" {
		t.Fatalf("error = %+v, want code=qcow2_header_invalid", terminal.Error)
	}
	if got := terminal.Error.Details["reason"]; got != "invalid_magic" {
		t.Errorf("details.reason = %v, want invalid_magic", got)
	}
}

func TestManager_ImportImage_DownloadFailedHTTPStatus(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	url := serveStatus(t, http.StatusNotFound)
	body := validQcow2Bytes()
	expectedSha := hexSHA256(body)

	task, err := m.ImportImage(t.Context(), poolName, ImportImageRequest{
		SourceURL:              url,
		ExpectedChecksumSHA256: expectedSha,
		Format:                 "qcow2",
	})
	if err != nil {
		t.Fatalf("ImportImage: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 2*time.Second)
	if terminal.Status != TaskStatusFailed {
		t.Fatalf("terminal.Status = %q, want %q", terminal.Status, TaskStatusFailed)
	}
	if terminal.Error == nil || terminal.Error.Code != "download_failed" {
		t.Fatalf("error = %+v, want code=download_failed", terminal.Error)
	}
	// downloadError preserves the source status в details.status.
	if got := terminal.Error.Details["status"]; got != float64(http.StatusNotFound) && got != http.StatusNotFound {
		// json.Marshal of map[string]any{int} вернёт float64 when later
		// decoded; здесь we hold the original Details map directly so
		// it stays an int — accept either to make the assertion robust.
		t.Errorf("details.status = %v (%T), want %d", got, got, http.StatusNotFound)
	}
}

func TestManager_DeleteImage_HappyPath(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	sha := hexSHA256(validQcow2Bytes())
	finalPath := filepath.Join(poolRoot, "templates", sha+".qcow2")
	if err := os.WriteFile(finalPath, validQcow2Bytes(), 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := m.DeleteImage(t.Context(), poolName, sha); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Errorf("file survived delete; err = %v", err)
	}
}

func TestManager_DeleteImage_Idempotent(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	// No file exists; delete must succeed silently.
	if err := m.DeleteImage(t.Context(), poolName, hexSHA256([]byte("x"))); err != nil {
		t.Errorf("DeleteImage on missing file: %v, want nil (idempotent)", err)
	}
}

func TestManager_DeleteImage_UnknownPool(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.DeleteImage(t.Context(), "unknown-pool", hexSHA256([]byte("x"))); !errors.Is(err, ErrPoolUnknown) {
		t.Errorf("err = %v, want ErrPoolUnknown", err)
	}
}

func TestManager_DeleteImage_InvalidChecksum(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	if err := m.DeleteImage(t.Context(), poolName, "deadbeef"); !errors.Is(err, ErrInvalidChecksumFormat) {
		t.Errorf("err = %v, want ErrInvalidChecksumFormat", err)
	}
}

func TestManager_ListImages_Empty(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	images, err := m.ListImages(t.Context(), poolName)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("len = %d, want 0", len(images))
	}
}

func TestManager_ListImages_Populated(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	body1 := validQcow2Bytes()
	sha1 := hexSHA256(body1)
	body2 := append(validQcow2Bytes(), 0x11)
	sha2 := hexSHA256(body2)

	templatesDir := filepath.Join(poolRoot, "templates")
	if err := os.WriteFile(filepath.Join(templatesDir, sha1+".qcow2"), body1, 0o640); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(templatesDir, sha2+".qcow2"), body2, 0o640); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	// Decoy entries that must be filtered out.
	if err := os.WriteFile(filepath.Join(templatesDir, "not-a-sha.qcow2"), []byte("x"), 0o640); err != nil {
		t.Fatalf("seed decoy filename: %v", err)
	}
	if err := os.WriteFile(filepath.Join(templatesDir, sha1+".raw"), []byte("x"), 0o640); err != nil {
		t.Fatalf("seed decoy ext: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(templatesDir, "subdir"), 0o750); err != nil {
		t.Fatalf("seed decoy dir: %v", err)
	}

	images, err := m.ListImages(t.Context(), poolName)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("len = %d, want 2; got = %+v", len(images), images)
	}
	// Sorted by checksum ascending.
	if images[0].ChecksumSHA256 != minStr(sha1, sha2) {
		t.Errorf("images[0].ChecksumSHA256 = %q, want %q (lex-min)", images[0].ChecksumSHA256, minStr(sha1, sha2))
	}
	for _, img := range images {
		if img.Format != "qcow2" {
			t.Errorf("img.Format = %q, want qcow2", img.Format)
		}
		if img.SizeBytes <= 0 {
			t.Errorf("img.SizeBytes = %d, want > 0", img.SizeBytes)
		}
		if img.LastUsedAt == nil {
			t.Errorf("img.LastUsedAt = nil, want pointer к file mtime")
		}
		if img.SourceURL != nil {
			t.Errorf("img.SourceURL = %v, want nil (agent does not track post-import)", *img.SourceURL)
		}
	}
}

func TestManager_ListImages_UnknownPool(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.ListImages(t.Context(), "unknown-pool"); !errors.Is(err, ErrPoolUnknown) {
		t.Errorf("err = %v, want ErrPoolUnknown", err)
	}
}

func TestValidateQcow2Magic(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid.qcow2")
	if err := os.WriteFile(valid, validQcow2Bytes(), 0o640); err != nil {
		t.Fatalf("seed valid: %v", err)
	}
	if err := validateQcow2Magic(valid); err != nil {
		t.Errorf("validateQcow2Magic(valid) = %v, want nil", err)
	}

	invalid := filepath.Join(dir, "invalid.qcow2")
	if err := os.WriteFile(invalid, invalidMagicBytes(), 0o640); err != nil {
		t.Fatalf("seed invalid: %v", err)
	}
	if err := validateQcow2Magic(invalid); err == nil {
		t.Errorf("validateQcow2Magic(invalid) = nil, want error")
	}

	tiny := filepath.Join(dir, "tiny.qcow2")
	if err := os.WriteFile(tiny, []byte{'Q', 'F'}, 0o640); err != nil {
		t.Fatalf("seed tiny: %v", err)
	}
	if err := validateQcow2Magic(tiny); err == nil {
		t.Errorf("validateQcow2Magic(tiny) = nil, want error (short read)")
	}

	missing := filepath.Join(dir, "missing.qcow2")
	if err := validateQcow2Magic(missing); err == nil {
		t.Errorf("validateQcow2Magic(missing) = nil, want error")
	}
}

func TestIsHexSHA256Lower(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"deadbeef", false},
		{"ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD", false},
		{"abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd", true},
		{"abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabc!", false},
		{"abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcde", false}, // 65 chars
	}
	for _, tc := range cases {
		if got := isHexSHA256Lower(tc.in); got != tc.want {
			t.Errorf("isHexSHA256Lower(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// minStr is а tiny helper для the table-driven list test.
func minStr(a, b string) string {
	if a < b {
		return a
	}
	return b
}
