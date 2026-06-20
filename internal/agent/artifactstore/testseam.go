// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactstore

// BlobPathForTest exposes a blob's on-disk path to tests in other packages that
// must construct corrupt-blob states the content-verified public API forbids.
func (s *Store) BlobPathForTest(digest string) string { return s.blobPath(digest) }

// SidecarPathForTest exposes a blob's sidecar path to tests in other packages
// that must construct corrupt-blob states the content-verified public API
// forbids.
func (s *Store) SidecarPathForTest(digest string) string { return s.sidecarPath(digest) }
