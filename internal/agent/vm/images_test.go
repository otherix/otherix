// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// mustNewForTesting builds an image store rooted under t.TempDir(), bypassing
// the production /var/lib/otherix/ allowlist so the pinned-image tests run on
// the macOS dev host.
func mustNewForTesting(t *testing.T) *artifactstore.Store {
	t.Helper()
	s, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("new image store: %v", err)
	}
	return s
}

func TestEnsureImageIntoPinnedDownloadsIntoImageStore(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(21)
	sum := shaHex(body)
	url := serve(t, body)

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	res, err := m.EnsureImageInto(context.Background(), poolName, url, sum, "qcow2", "", dst)
	if err != nil {
		t.Fatalf("EnsureImageInto(pinned miss) error = %v", err)
	}
	if res.SHA256 != sum {
		t.Errorf("res.SHA256 = %q, want %q", res.SHA256, sum)
	}
	if !m.ImageStore().Has(sum) {
		t.Errorf("image store missing digest %q after download", sum)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("clone destination not present: %v", err)
	}
}

func TestEnsureImageIntoPinnedHitClonesWithoutDownload(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(22)
	sum := shaHex(body)
	if err := m.ImageStore().Put(sum, bytes.NewReader(body)); err != nil {
		t.Fatalf("seed image store: %v", err)
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/ubuntu-24.04-arm64.img"

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	res, err := m.EnsureImageInto(context.Background(), poolName, url, sum, "qcow2", "", dst)
	if err != nil {
		t.Fatalf("EnsureImageInto(pinned hit) error = %v", err)
	}
	if res.SHA256 != sum {
		t.Errorf("res.SHA256 = %q, want %q", res.SHA256, sum)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("download count = %d, want 0 (cache hit must not download)", got)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("clone destination not present: %v", err)
	}
}

func TestEnsureImageIntoPinnedHitBumpsMtime(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(0x71)
	sum := shaHex(body)
	url := serve(t, body)
	if err := m.imageStore.Put(sum, bytes.NewReader(body)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	blobPath, err := m.imageStore.BlobPath(sum)
	if err != nil {
		t.Fatalf("BlobPath: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(blobPath, old, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	if _, err := m.EnsureImageInto(context.Background(), poolName, url, sum, "qcow2", "", dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	fi, statErr := os.Stat(blobPath)
	if statErr != nil {
		t.Fatalf("stat blob: %v", statErr)
	}
	if !fi.ModTime().After(old.Add(time.Hour)) {
		t.Errorf("cache-hit did not bump mtime: got %v, want newer than %v", fi.ModTime(), old)
	}
}

func TestEnsureImageIntoPinnedMissNudges(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(0x72)
	sum := shaHex(body)
	url := serve(t, body)
	nudged := 0
	m.SetImageEvictionNudge(func() { nudged++ })
	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	if _, err := m.EnsureImageInto(context.Background(), poolName, url, sum, "qcow2", "", dst); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if nudged != 1 {
		t.Errorf("nudge fired %d times after a download+Put, want 1", nudged)
	}
}

func TestEnsureImageIntoPinnedChecksumMismatchStoresNothing(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(23)
	sum := shaHex(body)
	url := serve(t, body)

	wrong := strings.Repeat("a", 64)
	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	_, err := m.EnsureImageInto(context.Background(), poolName, url, wrong, "qcow2", "", dst)
	if err == nil {
		t.Fatal("EnsureImageInto(pinned, url disagrees) error = nil, want checksum_mismatch")
	}
	var ce *ChecksumMismatchError
	if !errorAs(err, &ce) {
		t.Errorf("error = %v, want *ChecksumMismatchError", err)
	}
	if m.ImageStore().Has(wrong) {
		t.Errorf("image store holds the wrong digest after mismatch")
	}
	if m.ImageStore().Has(sum) {
		t.Errorf("image store holds the served digest after mismatch (nothing should be stored)")
	}
}

func TestEnsureUnpinnedHintHitClonesWithoutDownload(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(24)
	digest := shaHex(body)
	if err := m.ImageStore().Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatalf("seed image store: %v", err)
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/noble.img"

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	res, err := m.EnsureImageInto(context.Background(), poolName, url, "", "qcow2", digest, dst)
	if err != nil {
		t.Fatalf("EnsureImageInto = %v", err)
	}
	if res.SHA256 != digest {
		t.Errorf("res.SHA256 = %q, want %q", res.SHA256, digest)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("downloaded %d times, want 0 (hint hit must clone from cache)", got)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("disk not cloned: %v", err)
	}
	assertMetaSidecar(t, m.ImageStore().Root(), digest, url)
}

func TestEnsureUnpinnedNoHintDownloadsStoresReports(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(25)
	digest := shaHex(body)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/noble.img"

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	res, err := m.EnsureImageInto(context.Background(), poolName, url, "", "qcow2", "", dst)
	if err != nil {
		t.Fatalf("EnsureImageInto = %v", err)
	}
	if res.SHA256 != digest {
		t.Errorf("res.SHA256 = %q, want computed %q", res.SHA256, digest)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("downloaded %d times, want 1", got)
	}
	if !m.ImageStore().Has(digest) {
		t.Errorf("blob not stored under computed digest")
	}
	assertMetaSidecar(t, m.ImageStore().Root(), digest, url)
}

func TestEnsureUnpinnedStaleHintFallsBackToDownload(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(26)
	digest := shaHex(body)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/noble.img"

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	// hint points at a digest the store does NOT have and no peer populated.
	res, err := m.EnsureImageInto(context.Background(), poolName, url, "", "qcow2",
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0", dst)
	if err != nil {
		t.Fatalf("EnsureImageInto = %v", err)
	}
	if res.SHA256 != digest {
		t.Errorf("res.SHA256 = %q, want computed %q (stale hint must not pin)", res.SHA256, digest)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("downloaded %d times, want 1 (stale hint falls back to download)", got)
	}
}

// TestEnsureUnpinnedNoHintEvictionRaceRetryable pins the regression where the
// unpinned no-hint clone ran with no per-digest lock held: the Put nudges the
// eviction sweeper, which could delete the freshly stored blob before the clone
// opened it, turning a successful download into a terminal clone_failed. The
// nudge here deletes the just-Put blob (simulating eviction selecting it); with
// the fix the clone runs under the per-digest lock and re-checks presence, so
// the lost-race surfaces as a retryable error (never ErrCloneFailed).
func TestEnsureUnpinnedNoHintEvictionRaceRetryable(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(27)
	digest := shaHex(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/noble.img"

	// Simulate the eviction sweeper selecting and deleting the just-Put blob
	// the moment the Put nudges it - the lock is still held inside the Put, so
	// the Delete here lands after the Put returns and releases it but before the
	// clone takes the lock back.
	m.SetImageEvictionNudge(func() {
		if err := m.imageStore.Delete(digest); err != nil {
			t.Errorf("nudge delete: %v", err)
		}
	})

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	_, err := m.EnsureImageInto(context.Background(), poolName, url, "", "qcow2", "", dst)
	if err == nil {
		t.Fatal("EnsureImageInto = nil, want a retryable error (blob evicted before clone)")
	}
	if errors.Is(err, ErrCloneFailed) {
		t.Fatalf("error = %v, want retryable (not ErrCloneFailed); a delete-after-Put must not surface as terminal clone_failed", err)
	}
	if !strings.Contains(err.Error(), "retry") {
		t.Errorf("error = %v, want a message indicating retry", err)
	}
}

// assertMetaSidecar checks the human-readable <digest>.meta sidecar in the
// images-meta/ dir that sits as a sibling to the image store root.
func assertMetaSidecar(t *testing.T, imgRoot, digest, wantURL string) {
	t.Helper()
	metaPath := filepath.Join(filepath.Dir(imgRoot), "images-meta", digest+".meta")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read .meta sidecar: %v", err)
	}
	var meta struct {
		URL      string `json:"url"`
		Basename string `json:"basename"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatalf("parse .meta: %v", err)
	}
	if meta.URL != wantURL {
		t.Errorf(".meta url = %q, want %q", meta.URL, wantURL)
	}
}

// qcow2Body returns a minimal byte slice with the qcow2 magic prefix so
// validateQcow2Magic passes; the trailing bytes vary so distinct bodies
// hash distinctly.
func qcow2Body(tag byte) []byte {
	b := []byte{'Q', 'F', 'I', 0xfb}
	return append(b, tag, tag, tag, tag)
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/ubuntu-24.04-arm64.img"
}

// newImageTestManager constructs a Manager with one registered pool over
// a temp dir, returning the manager, the pool name, and the pool root.
func newImageTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The SSRF dial guard (blockLocalDial) refuses loopback, which is
	// exactly where httptest servers listen. Cache/hash tests are about the
	// pull policy, not the guard, so they opt out; the guard itself is pinned
	// by TestBlockLocalDial and TestEnsureImageBlocksLocalSource.
	m.imageDialControl = nil
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	return m, poolName, poolRoot
}

// errorAs is a thin wrapper over errors.As to keep the call sites terse.
func errorAs(err error, target any) bool {
	return errors.As(err, target)
}

func TestBasenameFromURL(t *testing.T) {
	got := basenameFromURL("https://host/path/ubuntu-24.04-minimal-cloudimg-arm64.img?x=1")
	if want := "ubuntu-24.04-minimal-cloudimg-arm64.img"; got != want {
		t.Errorf("basenameFromURL() = %q, want %q", got, want)
	}
}

func TestEnsureImageNameKeyedMissThenHit(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	body := qcow2Body(1)
	url := serve(t, body)

	res, err := m.EnsureImage(context.Background(), poolName, url, "", "qcow2")
	if err != nil {
		t.Fatalf("EnsureImage(miss) error = %v", err)
	}
	if res.SHA256 != shaHex(body) {
		t.Errorf("SHA256 = %q, want %q", res.SHA256, shaHex(body))
	}
	imgPath := filepath.Join(root, "images", "ubuntu-24.04-arm64.img")
	if _, err := os.Stat(imgPath); err != nil {
		t.Fatalf("cached image not present: %v", err)
	}
	sidecar, _ := os.ReadFile(imgPath + ".sha256")
	if string(sidecar) != shaHex(body) {
		t.Errorf("sidecar = %q, want %q", sidecar, shaHex(body))
	}

	// Second call: name-keyed HIT, server bytes ignored (dead url proves no re-download).
	res2, err := m.EnsureImage(context.Background(), poolName, "http://127.0.0.1:1/dead.img/ubuntu-24.04-arm64.img", "", "qcow2")
	if err != nil {
		t.Fatalf("EnsureImage(hit) error = %v", err)
	}
	if res2.SHA256 != res.SHA256 {
		t.Errorf("hit SHA256 = %q, want %q", res2.SHA256, res.SHA256)
	}
}

func TestEnsureImageShaMatchHit(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	body := qcow2Body(2)
	url := serve(t, body)
	want := shaHex(body)

	if _, err := m.EnsureImage(context.Background(), poolName, url, want, "qcow2"); err != nil {
		t.Fatalf("EnsureImage(first) error = %v", err)
	}
	if _, err := m.EnsureImage(context.Background(), poolName, "http://127.0.0.1:1/dead/ubuntu-24.04-arm64.img", want, "qcow2"); err != nil {
		t.Fatalf("EnsureImage(sha hit) error = %v", err)
	}
}

func TestEnsureImageShaMismatchOverwrites(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	old := qcow2Body(3)
	url := serve(t, old)
	if _, err := m.EnsureImage(context.Background(), poolName, url, shaHex(old), "qcow2"); err != nil {
		t.Fatalf("seed error = %v", err)
	}

	fresh := qcow2Body(4)
	freshURL := serve(t, fresh) // path basename is still ubuntu-24.04-arm64.img
	res, err := m.EnsureImage(context.Background(), poolName, freshURL, shaHex(fresh), "qcow2")
	if err != nil {
		t.Fatalf("EnsureImage(overwrite) error = %v", err)
	}
	if res.SHA256 != shaHex(fresh) {
		t.Errorf("overwrite SHA256 = %q, want %q", res.SHA256, shaHex(fresh))
	}
	imgPath := filepath.Join(root, "images", "ubuntu-24.04-arm64.img")
	got, _ := os.ReadFile(imgPath)
	if shaHex(got) != shaHex(fresh) {
		t.Errorf("cache content sha = %q, want %q", shaHex(got), shaHex(fresh))
	}
}

func TestEnsureImageURLDisagreesFailsClosed(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	body := qcow2Body(5)
	url := serve(t, body)
	wrongSHA := shaHex(qcow2Body(6))

	_, err := m.EnsureImage(context.Background(), poolName, url, wrongSHA, "qcow2")
	if err == nil {
		t.Fatalf("EnsureImage(url disagrees) error = nil, want checksum_mismatch")
	}
	var ce *ChecksumMismatchError
	if !errorAs(err, &ce) {
		t.Errorf("error = %v, want *ChecksumMismatchError", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "images", "ubuntu-24.04-arm64.img")); statErr == nil {
		t.Errorf("cache file written on mismatch; want nothing written")
	}
}

func TestEnsureImageOverwriteMismatchLeavesExistingFile(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	old := qcow2Body(7)
	url := serve(t, old)
	if _, err := m.EnsureImage(context.Background(), poolName, url, shaHex(old), "qcow2"); err != nil {
		t.Fatalf("seed error = %v", err)
	}

	// A re-ensure with a NEW expected sha whose URL serves DIFFERENT bytes
	// must fail closed and leave the existing cached file untouched.
	mismatchBody := qcow2Body(8)
	mismatchURL := serve(t, mismatchBody)
	wrongSHA := shaHex(qcow2Body(9)) // neither old nor served body
	_, err := m.EnsureImage(context.Background(), poolName, mismatchURL, wrongSHA, "qcow2")
	if err == nil {
		t.Fatalf("EnsureImage(stale slot, url disagrees) error = nil, want checksum_mismatch")
	}
	var ce *ChecksumMismatchError
	if !errorAs(err, &ce) {
		t.Errorf("error = %v, want *ChecksumMismatchError", err)
	}
	imgPath := filepath.Join(root, "images", "ubuntu-24.04-arm64.img")
	got, _ := os.ReadFile(imgPath)
	if shaHex(got) != shaHex(old) {
		t.Errorf("existing cache content changed on failed re-download; sha = %q, want %q", shaHex(got), shaHex(old))
	}
}

func TestEnsureImageUnknownPool(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.imageDialControl = nil // see newImageTestManager
	if _, err := m.EnsureImage(context.Background(), "unknown-pool", "http://x.invalid/a.img", "", "qcow2"); !errors.Is(err, ErrPoolUnknown) {
		t.Errorf("err = %v, want ErrPoolUnknown", err)
	}
}

func TestEnsureImageValidationErrors(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	cases := []struct {
		name        string
		url         string
		expectedSHA string
		format      string
		want        error
	}{
		{"unsupported format", "http://x.invalid/a.img", "", "raw", ErrUnsupportedFormat},
		{"missing source url", "", "", "qcow2", ErrMissingSourceURL},
		{"checksum too short", "http://x.invalid/a.img", "deadbeef", "qcow2", ErrInvalidChecksumFormat},
		{
			"checksum uppercase",
			"http://x.invalid/a.img",
			"ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD",
			"qcow2",
			ErrInvalidChecksumFormat,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.EnsureImage(context.Background(), poolName, tc.url, tc.expectedSHA, tc.format)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestValidateQcow2Magic(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid.qcow2")
	if err := os.WriteFile(valid, qcow2Body(0), 0o640); err != nil {
		t.Fatalf("seed valid: %v", err)
	}
	if err := validateQcow2Magic(valid); err != nil {
		t.Errorf("validateQcow2Magic(valid) = %v, want nil", err)
	}

	invalid := filepath.Join(dir, "invalid.qcow2")
	if err := os.WriteFile(invalid, []byte{'N', 'O', 'P', 'E', 0, 0, 0, 0}, 0o640); err != nil {
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

func TestValidImageBasename(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ubuntu-24.04-arm64.img", true},
		{"a.qcow2", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a/b", false},
		{"../escape", false},
		{"images/../..", false},
	}
	for _, tc := range cases {
		if got := validImageBasename(tc.in); got != tc.want {
			t.Errorf("validImageBasename(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEnsureImageRejectsTraversalBasename(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	// A source URL whose path basename collapses to ".." must be rejected
	// before any filesystem action: filepath.Join(root, "images", "..")
	// would otherwise resolve to the pool root.
	_, err := m.EnsureImage(context.Background(), poolName, "http://host/foo/..", "", "qcow2")
	if !errors.Is(err, ErrInvalidImageBasename) {
		t.Errorf("EnsureImage(traversal) error = %v, want ErrInvalidImageBasename", err)
	}
	// AddPool pre-creates the images/ dir; the reject must write no file
	// into it (no traversal-collapsed write landed in the pool root).
	imagesDir := filepath.Join(root, "images")
	entries, statErr := os.ReadDir(imagesDir)
	if statErr != nil {
		t.Fatalf("read images dir: %v", statErr)
	}
	if len(entries) != 0 {
		t.Errorf("images dir holds %d entries after a rejected traversal basename; want none", len(entries))
	}
}

// TestBlockLocalDial pins the SSRF guard's address taxonomy:
// loopback, link-local (incl. the cloud IMDS at 169.254.169.254), multicast,
// and unspecified addresses are refused; RFC1918 + ULA private addresses
// pass (operators host image mirrors on private networks); public addresses
// pass; non-IP hosts are refused (the guard runs post-DNS, so a hostname
// reaching it means the dialer is miswired).
func TestBlockLocalDial(t *testing.T) {
	cases := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"loopback v4", "127.0.0.1:80", true},
		{"loopback v6", "[::1]:443", true},
		{"private 10/8", "10.0.0.5:443", false},
		{"private 192.168/16", "192.168.1.1:80", false},
		{"private 172.16/12", "172.16.0.1:8080", false},
		{"link-local IMDS", "169.254.169.254:80", true},
		{"link-local v6", "[fe80::1]:80", true},
		{"ULA v6", "[fd00::1]:443", false},
		{"multicast v4", "224.0.0.1:80", true},
		{"unspecified v4", "0.0.0.0:80", true},
		{"unspecified v6", "[::]:80", true},
		{"non-ip host", "internal-svc:80", true},
		{"missing port", "127.0.0.1", true},
		{"public v4", "93.184.216.34:443", false},
		{"public v6", "[2606:2800:220:1:248:1893:25c8:1946]:443", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := blockLocalDial("tcp4", tc.address, nil)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("blockLocalDial(%q) error = %v, wantErr %v", tc.address, err, tc.wantErr)
			}
		})
	}
}

// TestEnsureImageBlocksLocalSource proves the guard is wired into the
// real fetch path: a Manager fresh from New (guard NOT nilled, unlike
// newImageTestManager) must refuse to download from an httptest server,
// which listens on loopback.
func TestEnsureImageBlocksLocalSource(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	url := serve(t, qcow2Body(0x44))
	_, err = m.EnsureImage(context.Background(), poolName, url, "", "qcow2")
	if err == nil {
		t.Fatal("EnsureImage(loopback source) error = nil, want SSRF guard rejection")
	}
	if !strings.Contains(err.Error(), "node-local or special-use address") {
		t.Errorf("EnsureImage(loopback source) error = %v, want it to name the blocked address", err)
	}
	if _, statErr := os.Stat(filepath.Join(poolRoot, "images", "ubuntu-24.04-arm64.img")); statErr == nil {
		t.Errorf("cache file written for a guard-blocked download; want nothing written")
	}
}

// TestEnsureImageExceedsMaxBytes pins the download byte cap: a body larger
// than maxImageBytes fails closed (no truncated file enters the cache). The
// cap is a package var solely so this test can lower it; nothing in this
// package runs t.Parallel, so the override cannot race.
func TestEnsureImageExceedsMaxBytes(t *testing.T) {
	old := maxImageBytes
	maxImageBytes = 16
	t.Cleanup(func() { maxImageBytes = old })

	m, poolName, root := newImageTestManager(t)
	body := append(qcow2Body(0x55), make([]byte, 32)...) // 40 bytes > 16-byte cap
	url := serve(t, body)

	_, err := m.EnsureImage(context.Background(), poolName, url, "", "qcow2")
	if err == nil {
		t.Fatal("EnsureImage(over cap) error = nil, want size-cap rejection")
	}
	if !strings.Contains(err.Error(), "exceeds max size") {
		t.Errorf("EnsureImage(over cap) error = %v, want it to report the size cap", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "images", "ubuntu-24.04-arm64.img")); statErr == nil {
		t.Errorf("cache file written for an over-cap download; want nothing written")
	}
}

func TestDeleteImageRejectsTraversalBasename(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	// Seed a real cached image so the pool root is non-empty.
	body := qcow2Body(9)
	if _, err := m.EnsureImage(context.Background(), poolName, serve(t, body), "", "qcow2"); err != nil {
		t.Fatalf("seed EnsureImage error = %v", err)
	}
	// A crafted delete key of ".." must be rejected, not collapse onto the
	// pool root via filepath.Join(root, "images", "..").
	if err := m.DeleteImage(context.Background(), poolName, ".."); !errors.Is(err, ErrInvalidChecksumFormat) {
		t.Errorf("DeleteImage(\"..\") error = %v, want ErrInvalidChecksumFormat", err)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Errorf("pool root removed by a crafted delete key: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "images")); statErr != nil {
		t.Errorf("images dir removed by a crafted delete key: %v", statErr)
	}
}

// TestEnsureImageIntoRehashesTamperedHit drives R2-L5: on a pinned-digest
// cache HIT whose file bytes were tampered out-of-band (the .sha256 sidecar
// still says S), EnsureImageInto must NOT clone the tampered bytes. It
// re-hashes the cache file, sees the disagreement, re-downloads the correct
// bytes from the source, and the cloned dst matches S.
func TestEnsureImageIntoRehashesTamperedHit(t *testing.T) {
	m, poolName, root := newImageTestManager(t)
	body := qcow2Body(11)
	want := shaHex(body)
	url := serve(t, body)

	dst1 := filepath.Join(t.TempDir(), "disk1.qcow2")
	if _, err := m.EnsureImageInto(context.Background(), poolName, url, want, "qcow2", "", dst1); err != nil {
		t.Fatalf("EnsureImageInto(seed) error = %v", err)
	}
	got1, _ := os.ReadFile(dst1)
	if shaHex(got1) != want {
		t.Fatalf("seed clone sha = %q, want %q", shaHex(got1), want)
	}

	// Tamper the cache file bytes, leaving the sidecar claiming the original S.
	imgPath := filepath.Join(root, "images", "ubuntu-24.04-arm64.img")
	if err := os.WriteFile(imgPath, qcow2Body(99), 0o600); err != nil {
		t.Fatalf("tamper cache file: %v", err)
	}
	sidecar, _ := os.ReadFile(imgPath + ".sha256")
	if strings.TrimSpace(string(sidecar)) != want {
		t.Fatalf("sidecar changed by tamper; got %q, want %q", sidecar, want)
	}

	dst2 := filepath.Join(t.TempDir(), "disk2.qcow2")
	if _, err := m.EnsureImageInto(context.Background(), poolName, url, want, "qcow2", "", dst2); err != nil {
		t.Fatalf("EnsureImageInto(tampered hit) error = %v", err)
	}
	got2, _ := os.ReadFile(dst2)
	if shaHex(got2) != want {
		t.Errorf("clone of tampered hit sha = %q, want %q (re-download did not heal the cache)", shaHex(got2), want)
	}
	// The cache file itself must now hold the correct bytes (re-downloaded).
	cached, _ := os.ReadFile(imgPath)
	if shaHex(cached) != want {
		t.Errorf("cache file sha after re-download = %q, want %q", shaHex(cached), want)
	}
}

func TestEnsureImageIntoUnpinnedCoalescesConcurrentSameURL(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	m.imageDialControl = nil // allow loopback httptest dials

	body := qcow2Body(0x5a)
	wantSHA := shaHex(body)

	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release // block so concurrent callers pile into the in-flight singleflight call
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/noble-minimal-cloudimg-arm64.img"

	const n = 5
	dsts := make([]string, n)
	results := make([]EnsureResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		dsts[i] = filepath.Join(t.TempDir(), "disk.qcow2")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.EnsureImageInto(context.Background(), poolName, url, "", "qcow2", "", dsts[i])
		}(i)
	}
	// Canonical singleflight test barrier: give every goroutine time to reach
	// the in-flight Do before the leader's download completes.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("download count = %d, want 1 (concurrent same-URL imports must coalesce)", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d error = %v", i, errs[i])
			continue
		}
		if results[i].SHA256 != wantSHA {
			t.Errorf("caller %d SHA256 = %q, want %q", i, results[i].SHA256, wantSHA)
		}
		if _, err := os.Stat(dsts[i]); err != nil {
			t.Errorf("caller %d clone destination missing: %v", i, err)
		}
	}
	if !m.ImageStore().Has(wantSHA) {
		t.Errorf("image store missing digest %q after coalesced download", wantSHA)
	}
}

func discardImageLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSweepLegacyBasenameCacheRemovesRegularFilesKeepsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), imagesSubdir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"noble.img", "noble.img.sha256", "jammy.img", "jammy.img.sha256"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	got := sweepLegacyBasenameCache(dir, discardImageLogger())
	if got != 4 {
		t.Errorf("removed = %d, want 4", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("images dir must survive the sweep: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("images dir not emptied: %d entries remain", len(ents))
	}
}

func TestSweepLegacyBasenameCacheAbsentDirIsNoop(t *testing.T) {
	got := sweepLegacyBasenameCache(filepath.Join(t.TempDir(), "nonexistent"), discardImageLogger())
	if got != 0 {
		t.Errorf("removed = %d, want 0 for an absent directory", got)
	}
}

func TestSweepLegacyBasenameCacheSkipsSubdirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), imagesSubdir)
	if err := os.MkdirAll(filepath.Join(dir, "keep-me-subdir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noble.img"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	got := sweepLegacyBasenameCache(dir, discardImageLogger())
	if got != 1 {
		t.Errorf("removed = %d, want 1 (the regular file only)", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep-me-subdir")); err != nil {
		t.Errorf("subdirectory must not be removed: %v", err)
	}
}

func TestAddPoolSweepsLegacyBasenameCacheOnFirstRegistration(t *testing.T) {
	m, _, _ := newImageTestManager(t)
	root := t.TempDir()
	imagesDir := filepath.Join(root, imagesSubdir)
	if err := os.MkdirAll(imagesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(imagesDir, "stale.img")
	if err := os.WriteFile(legacy, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := m.AddPool("sweep-pool", root); err != nil {
		t.Fatalf("AddPool = %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy basename cache file not swept on first AddPool")
	}
	if _, err := os.Stat(imagesDir); err != nil {
		t.Errorf("images dir must survive AddPool sweep: %v", err)
	}
}

// TestImageScratchDirIsImageStoreSibling pins the managed scratch dir to a sibling
// of the image store root so the agent boot sweep can reclaim partial downloads.
func TestImageScratchDirIsImageStoreSibling(t *testing.T) {
	m, _, _ := newImageTestManager(t)
	if m.imageScratchDir() != "" {
		t.Errorf("imageScratchDir with no image store = %q, want empty", m.imageScratchDir())
	}
	m.imageStore = mustNewForTesting(t)
	want := filepath.Join(filepath.Dir(m.imageStore.Root()), "image-scratch")
	if got := m.imageScratchDir(); got != want {
		t.Errorf("imageScratchDir() = %q, want %q", got, want)
	}
}

// TestSweepImageScratchClearsLeftovers confirms SweepImageScratch removes leftover
// scratch dirs and leaves an empty image-scratch dir in place.
func TestSweepImageScratchClearsLeftovers(t *testing.T) {
	m, _, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)

	scratchRoot := m.imageScratchDir()
	leftover := filepath.Join(scratchRoot, "otherix-image-stale")
	if err := os.MkdirAll(leftover, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "download"), []byte("partial"), 0o640); err != nil {
		t.Fatal(err)
	}

	m.SweepImageScratch()

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("leftover scratch dir not removed by SweepImageScratch")
	}
	info, err := os.Stat(scratchRoot)
	if err != nil || !info.IsDir() {
		t.Errorf("image-scratch dir must be present and empty after sweep: stat err=%v", err)
	}
	entries, err := os.ReadDir(scratchRoot)
	if err != nil {
		t.Fatalf("ReadDir(scratchRoot) = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("image-scratch dir not empty after sweep: %d entries", len(entries))
	}
}

// TestPinnedDownloadStagesUnderManagedScratch confirms a pinned download lands its
// scratch under the managed image-scratch dir (not the OS temp dir) and still
// completes end to end through the httptest harness.
func TestPinnedDownloadStagesUnderManagedScratch(t *testing.T) {
	m, poolName, _ := newImageTestManager(t)
	m.imageStore = mustNewForTesting(t)
	body := qcow2Body(91)
	sum := shaHex(body)
	url := serve(t, body)

	dst := filepath.Join(t.TempDir(), "disk.qcow2")
	if _, err := m.EnsureImageInto(context.Background(), poolName, url, sum, "qcow2", "", dst); err != nil {
		t.Fatalf("EnsureImageInto(pinned miss) error = %v", err)
	}
	if !m.ImageStore().Has(sum) {
		t.Errorf("image store missing digest after download")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("clone destination not present: %v", err)
	}
	// The managed scratch root must exist (the download created it) and the
	// per-download temp must be gone (defer cleanup).
	if _, err := os.Stat(m.imageScratchDir()); err != nil {
		t.Errorf("managed scratch root not created by download: %v", err)
	}
	entries, err := os.ReadDir(m.imageScratchDir())
	if err != nil {
		t.Fatalf("ReadDir(scratch) = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("download temp not cleaned from managed scratch: %d entries", len(entries))
	}
}
