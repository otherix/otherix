// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// newTestManagerWithArtifactsRoot builds a Manager from the standard test
// config (newTestConfig) with Artifacts.Root overridden to root, so the
// image-store wiring can be exercised with both a set and an empty root.
func newTestManagerWithArtifactsRoot(t *testing.T, root string) (*Manager, error) {
	t.Helper()
	cfg, _, _ := newTestConfig(t)
	cfg.Artifacts.Root = root
	return New(cfg, &netfabric.FakeFabric{}, discardLogger())
}

// allowlistArtifactsRoot returns a unique artifacts root under the fixed
// /var/lib/otherix/ allowlist prefix that the production artifactstore.New
// enforces. The manager constructor routes both stores through that
// production constructor, so a t.TempDir() root cannot be used; the test
// skips when the allowlist parent is not writable (e.g. a sandboxed CI run).
func allowlistArtifactsRoot(t *testing.T) string {
	t.Helper()
	base := filepath.Join("/var/lib/otherix", "test-imagestore")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Skipf("cannot create artifacts allowlist dir %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, "case")
	if err != nil {
		t.Skipf("cannot create artifacts temp dir under %s: %v", base, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "artifacts")
}

func TestImageStoreRootIsSiblingOfArtifacts(t *testing.T) {
	artRoot := allowlistArtifactsRoot(t)
	m, err := newTestManagerWithArtifactsRoot(t, artRoot)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if m.ImageStore() == nil {
		t.Fatalf("ImageStore() = nil, want non-nil when artifacts root is set")
	}
	if m.ImageStore() == m.ArtifactStore() {
		t.Errorf("ImageStore() and ArtifactStore() are the same instance, want distinct")
	}
	wantRoot := filepath.Join(filepath.Dir(artRoot), "images")
	if got := m.ImageStore().Root(); got != wantRoot {
		t.Errorf("ImageStore().Root() = %q, want %q (sibling of artifacts root)", got, wantRoot)
	}
}

func TestImageStoreNilWhenArtifactsRootEmpty(t *testing.T) {
	m, err := newTestManagerWithArtifactsRoot(t, "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if m.ImageStore() != nil {
		t.Errorf("ImageStore() = non-nil, want nil when artifacts root is empty")
	}
}
