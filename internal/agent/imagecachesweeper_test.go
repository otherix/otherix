// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/config"
)

// digestA, digestB, digestC are valid 64-char lowercase-hex content addresses so
// the store's List/Has/Delete (all of which validate the digest) accept them.
var (
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	digestC = strings.Repeat("c", 64)
)

func sweeperDiscardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedBlob writes a blob file of size bytes (plus its sidecar) directly under the
// store's blobs dir with the given mtime. It bypasses Store.Put on purpose: Put
// verifies the streamed content hashes to digest, but these tests want arbitrary
// content under a chosen hex digest name. List/Has/Delete only require a hex name
// and a regular file, so the seeded blob is a faithful stand-in.
func seedBlob(t *testing.T, store *artifactstore.Store, digest string, size int64, mtime time.Time) {
	t.Helper()
	blobsDir := filepath.Join(store.Root(), "blobs")
	if err := os.MkdirAll(blobsDir, 0o750); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	blobPath := filepath.Join(blobsDir, digest)
	if err := os.WriteFile(blobPath, make([]byte, size), 0o640); err != nil {
		t.Fatalf("write blob %s: %v", digest, err)
	}
	if err := os.WriteFile(blobPath+".sha256", []byte(digest), 0o640); err != nil {
		t.Fatalf("write sidecar %s: %v", digest, err)
	}
	if err := os.Chtimes(blobPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", digest, err)
	}
}

func newTestStore(t *testing.T) *artifactstore.Store {
	t.Helper()
	store, err := artifactstore.NewForTesting(t.TempDir())
	if err != nil {
		t.Fatalf("NewForTesting: %v", err)
	}
	return store
}

// alwaysLock is a tryLock stub that always grants the lock.
func alwaysLock(string) (func(), bool) { return func() {}, true }

// neverFree is a freeBytes stub that should never be consulted in a test.
func neverFree(string) (uint64, error) {
	panic("freeBytes called unexpectedly")
}

func TestSweepEvictsColdestToMaxBytes(t *testing.T) {
	store := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	seedBlob(t, store, digestA, 100, base)                  // coldest
	seedBlob(t, store, digestB, 100, base.Add(time.Minute)) // mid
	seedBlob(t, store, digestC, 100, base.Add(2*time.Minute))

	sw := newImageCacheSweeper(store, alwaysLock, neverFree,
		config.ImageCacheConfig{MaxBytes: 250}, nil, nil, sweeperDiscardLog())
	sw.sweep(context.Background())

	if store.Has(digestA) {
		t.Errorf("coldest blob %s still present, want evicted", "a*64")
	}
	if !store.Has(digestB) {
		t.Errorf("blob %s evicted, want kept", "b*64")
	}
	if !store.Has(digestC) {
		t.Errorf("blob %s evicted, want kept", "c*64")
	}
}

func TestSweepFreeFloorTriggersUnderMaxBytes(t *testing.T) {
	store := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	// 200-byte blobs so the 150-byte deficit (MinFreeBytes 200 - free 50) is met
	// by evicting the coldest alone.
	seedBlob(t, store, digestA, 200, base)                  // coldest
	seedBlob(t, store, digestB, 200, base.Add(time.Minute)) // warmer

	freeStub := func(string) (uint64, error) { return 50, nil }
	sw := newImageCacheSweeper(store, alwaysLock, freeStub,
		config.ImageCacheConfig{MaxBytes: 0, MinFreeBytes: 200}, nil, nil, sweeperDiscardLog())
	sw.sweep(context.Background())

	if store.Has(digestA) {
		t.Errorf("coldest blob %s still present, want evicted under free-space floor", "a*64")
	}
	if !store.Has(digestB) {
		t.Errorf("blob %s evicted, want kept", "b*64")
	}
}

func TestSweepSkipsLockedCandidate(t *testing.T) {
	store := newTestStore(t)
	base := time.Now().Add(-time.Hour)
	seedBlob(t, store, digestA, 100, base)                  // coldest, but locked (mid-clone)
	seedBlob(t, store, digestB, 100, base.Add(time.Minute)) // warmer, unlocked

	// tryLock denies the coldest digest (an in-flight clone holds it) and grants
	// every other digest. The sweeper must never delete a blob it cannot lock.
	lockStub := func(digest string) (func(), bool) {
		if digest == digestA {
			return nil, false
		}
		return func() {}, true
	}
	sw := newImageCacheSweeper(store, lockStub, neverFree,
		config.ImageCacheConfig{MaxBytes: 150}, nil, nil, sweeperDiscardLog())
	sw.sweep(context.Background())

	if !store.Has(digestA) {
		t.Errorf("locked blob %s was evicted, want kept (in-flight clone must survive)", "a*64")
	}
	if store.Has(digestB) {
		t.Errorf("unlocked blob %s still present, want evicted", "b*64")
	}
}

func TestSweepNoopUnderCeilings(t *testing.T) {
	store := newTestStore(t)
	seedBlob(t, store, digestA, 100, time.Now().Add(-time.Hour))

	sw := newImageCacheSweeper(store, alwaysLock, neverFree,
		config.ImageCacheConfig{MaxBytes: 1000, MinFreeBytes: 0}, nil, nil, sweeperDiscardLog())
	sw.sweep(context.Background())

	if !store.Has(digestA) {
		t.Errorf("blob %s evicted, want kept (under ceilings)", "a*64")
	}
}

func TestSweepStatfsErrorIsFailOpen(t *testing.T) {
	store := newTestStore(t)
	seedBlob(t, store, digestA, 100, time.Now().Add(-time.Hour))

	freeErr := func(string) (uint64, error) { return 0, os.ErrPermission }
	sw := newImageCacheSweeper(store, alwaysLock, freeErr,
		config.ImageCacheConfig{MaxBytes: 0, MinFreeBytes: 200}, nil, nil, sweeperDiscardLog())
	sw.sweep(context.Background()) // must not panic

	if !store.Has(digestA) {
		t.Errorf("blob %s evicted despite statfs error, want kept (fail open)", "a*64")
	}
}

func TestSweepDisabledWhenBothCeilingsZero(t *testing.T) {
	store := newTestStore(t)
	seedBlob(t, store, digestA, 100, time.Now().Add(-time.Hour))

	sw := newImageCacheSweeper(store, alwaysLock, neverFree,
		config.ImageCacheConfig{MaxBytes: 0, MinFreeBytes: 0}, nil, nil, sweeperDiscardLog())
	sw.sweep(context.Background())

	if !store.Has(digestA) {
		t.Errorf("blob %s evicted, want kept (eviction disabled)", "a*64")
	}
}
