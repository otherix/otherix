// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

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
	if _, statErr := os.Stat(filepath.Join(root, "images")); statErr == nil {
		t.Errorf("images dir created on a rejected traversal basename; want no filesystem action")
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
