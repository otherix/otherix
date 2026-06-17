// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// snapshotsStagingSubdir is the per-pool scratch dir under snapshots/ where a
// blob is materialized + hashed before its content-addressed name is known.
// The final atomic rename lands the blob beside it under the same snapshots/
// dir, so the rename is intra-directory (same filesystem, truly atomic).
const snapshotsStagingSubdir = ".staging"

// BlobResult describes one produced content-addressed snapshot disk blob:
// its sha256 digest (the content address), the final on-disk path
// ({snapshotsDir}/{sha}.qcow2), and its byte size.
type BlobResult struct {
	SHA256    string
	Path      string
	SizeBytes int64
}

// SnapshotBlob is the agent-internal view of one cached snapshot blob,
// projected by ListSnapshots from a filesystem walk of {poolRoot}/snapshots/.
type SnapshotBlob struct {
	SHA256    string
	Path      string
	SizeBytes int64
}

// convertFunc materializes a standalone qcow2 copy of src at dst. Production
// passes qemu.ConvertTo; tests pass a verbatim copy. It is the seam that
// keeps produceBlob's durability-critical hash/rename/sidecar tail unit
// testable without shelling out to qemu-img.
type convertFunc func(ctx context.Context, src, dst string) error

// produceBlob materializes srcDisk (a backup-target qcow2) into a
// content-addressed blob under snapshotsDir, mirroring the image cache's
// download-into-cache tail with a local convert source instead of HTTP:
//
//  1. convert srcDisk into {snapshotsDir}/.staging/<rand>.qcow2 (fresh,
//     standalone qcow2);
//  2. hash the staging file and validate its qcow2 magic;
//  3. if {snapshotsDir}/{sha}.qcow2 already exists with a matching sidecar,
//     reuse it (idempotent; drop the staging file);
//  4. otherwise atomically rename the staging file to {snapshotsDir}/{sha}.qcow2
//     then write the {sha}.qcow2.sha256 sidecar.
//
// The hash is computed BEFORE the final name is known and the blob is renamed
// into place only after it validates, so a crash never leaves a partial
// {sha}.qcow2: the staging file is the only thing that can be left behind, and
// it is cleaned up on the way out.
func produceBlob(ctx context.Context, srcDisk, snapshotsDir string, convert convertFunc) (BlobResult, error) {
	stagingDir := filepath.Join(snapshotsDir, snapshotsStagingSubdir)
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return BlobResult{}, fmt.Errorf("create snapshots staging dir: %v", err)
	}
	tempPath := filepath.Join(stagingDir, uuid.NewString()+".qcow2")
	defer func() { _ = os.Remove(tempPath) }()

	if err := convert(ctx, srcDisk, tempPath); err != nil {
		return BlobResult{}, fmt.Errorf("convert %s into staging blob: %v", srcDisk, err)
	}
	if err := validateQcow2Magic(tempPath); err != nil {
		return BlobResult{}, fmt.Errorf("qcow2_header_invalid: %v", err)
	}
	sha, err := hashFile(tempPath)
	if err != nil {
		return BlobResult{}, fmt.Errorf("hash staging blob: %v", err)
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return BlobResult{}, fmt.Errorf("stat staging blob: %v", err)
	}
	size := info.Size()

	blobPath := filepath.Join(snapshotsDir, sha+".qcow2")
	sidecarPath := blobPath + ".sha256"

	// Idempotent reuse: a prior identical blob already present (with a
	// well-formed matching sidecar) is the same content - drop the staging
	// copy and reuse it.
	if cachedSHA, cachedSize, present := readCachedImage(blobPath, sidecarPath); present && cachedSHA == sha {
		return BlobResult{SHA256: sha, Path: blobPath, SizeBytes: cachedSize}, nil
	}

	if err := os.Rename(tempPath, blobPath); err != nil {
		return BlobResult{}, fmt.Errorf("atomic rename to snapshots: %v", err)
	}
	if err := os.WriteFile(sidecarPath, []byte(sha), 0o644); err != nil { //nolint:gosec // sidecar is non-secret metadata
		return BlobResult{}, fmt.Errorf("write snapshot sidecar: %v", err)
	}
	return BlobResult{SHA256: sha, Path: blobPath, SizeBytes: size}, nil
}

// ListSnapshots walks snapshotsDir and returns the inventory of content-
// addressed snapshot blobs: every {sha}.qcow2 file paired with the digest
// read from its sidecar. Mirrors ListImages: files lacking a well-formed
// sidecar are skipped (partial produce or scratch), the .staging subdir is
// skipped, and an absent snapshotsDir yields an empty inventory and nil error.
func ListSnapshots(snapshotsDir string) ([]SnapshotBlob, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}

	blobs := make([]SnapshotBlob, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sha256") {
			continue
		}
		blobPath := filepath.Join(snapshotsDir, name)
		sha, size, present := readCachedImage(blobPath, blobPath+".sha256")
		if !present {
			continue
		}
		blobs = append(blobs, SnapshotBlob{
			SHA256:    sha,
			Path:      blobPath,
			SizeBytes: size,
		})
	}
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].SHA256 < blobs[j].SHA256
	})
	return blobs, nil
}
