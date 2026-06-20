// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
)

// imageURLDigest is the value stored under image_url_digests/<sha256hex(url)>.
// It maps a source image URL to the content digest a node most recently
// imported from it. The create path consults it to peer-pull an unpinned image
// by its resolved digest. The URL is hashed into the key so an arbitrary URL is
// a safe, fixed-width etcd key. The value is a last-writer-wins pointer: an
// import with an older imported_at never clobbers a newer one (out-of-order
// create completions cannot regress the pointer).
type imageURLDigest struct {
	URL        string    `json:"url"`
	Digest     string    `json:"digest"`
	SizeBytes  int64     `json:"size_bytes"`
	ImportedAt time.Time `json:"imported_at"`
	ImportedBy uuid.UUID `json:"imported_by_node"`
}

func imageURLDigestKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return etcd.Key("image_url_digests", hex.EncodeToString(sum[:]))
}

// UpsertImageURLDigest records that node imported url and resolved it to digest
// at importedAt. Last-writer-wins by importedAt: a write whose importedAt is not
// strictly newer than the stored entry is ignored, so a slow or stale create
// cannot regress the pointer.
func (s *Store) UpsertImageURLDigest(ctx context.Context, url, digest string, sizeBytes int64, importedAt time.Time, node uuid.UUID) error {
	key := imageURLDigestKey(url)
	var existing imageURLDigest
	found, err := s.c.GetJSON(ctx, key, &existing)
	if err != nil {
		return err
	}
	if found && !importedAt.After(existing.ImportedAt) {
		return nil
	}
	return s.c.PutJSON(ctx, key, imageURLDigest{
		URL:        url,
		Digest:     digest,
		SizeBytes:  sizeBytes,
		ImportedAt: importedAt.UTC(),
		ImportedBy: node,
	})
}

// ImageURLDigest returns the most recently imported digest for url, or ok=false
// when no node has imported it yet.
func (s *Store) ImageURLDigest(ctx context.Context, url string) (digest string, ok bool, err error) {
	var v imageURLDigest
	found, err := s.c.GetJSON(ctx, imageURLDigestKey(url), &v)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	return v.Digest, true, nil
}
