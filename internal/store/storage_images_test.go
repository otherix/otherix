// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/store"
)

// seedTemplateForImages inserts a minimal template owned by owner.
// Used by storage_images tests to provide the FK target.
func seedTemplateForImages(t *testing.T, ctx context.Context, s *store.Store, owner uuid.UUID, prefix string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Queries().CreateTemplate(ctx,
		defaultTemplateParams(id, owner, uniqueTemplateName(prefix)),
	); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	return id
}

// seedPoolForImages inserts a minimal storage_pool on node. Returns the
// pool id.
func seedPoolForImages(t *testing.T, ctx context.Context, s *store.Store, node uuid.UUID, prefix string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Queries().CreateStoragePool(ctx,
		defaultPoolParams(id, node, uniquePoolName(prefix)),
	); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return id
}

// imageSHA256 returns a 64-char lowercase-hex sha256 stand-in derived
// from a single byte. Matches the storage_images.checksum_sha256 CHECK
// constraint without requiring crypto/sha256 in test code.
func imageSHA256(b byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	hi := hex[b>>4]
	lo := hex[b&0x0f]
	for i := 0; i < 32; i++ {
		out[2*i] = hi
		out[2*i+1] = lo
	}
	return string(out)
}

func TestStorageImagesCreateGetRoundTrip(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "img-rt")
	pool := seedPoolForImages(t, ctx, s, node, "img-rt")

	id := uuid.New()
	sha := imageSHA256(0xab)
	created, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             id,
		TemplateID:     tpl,
		PoolID:         pool,
		ChecksumSha256: sha,
		SizeBytes:      4096,
		Format:         "qcow2",
	})
	if err != nil {
		t.Fatalf("CreateStorageImage: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID = %v, want %v", created.ID, id)
	}
	if created.ChecksumSha256 != sha {
		t.Errorf("created.ChecksumSha256 = %q, want %q", created.ChecksumSha256, sha)
	}
	if created.SizeBytes != 4096 {
		t.Errorf("created.SizeBytes = %d, want 4096", created.SizeBytes)
	}
	if created.Format != "qcow2" {
		t.Errorf("created.Format = %q, want qcow2", created.Format)
	}

	got, err := s.Queries().GetStorageImageByID(ctx, id)
	if err != nil {
		t.Fatalf("GetStorageImageByID: %v", err)
	}
	if got.ID != id {
		t.Errorf("got.ID = %v, want %v", got.ID, id)
	}

	gotByPair, err := s.Queries().GetStorageImageByTemplateAndPool(ctx,
		store.GetStorageImageByTemplateAndPoolParams{TemplateID: tpl, PoolID: pool},
	)
	if err != nil {
		t.Fatalf("GetStorageImageByTemplateAndPool: %v", err)
	}
	if gotByPair.ID != id {
		t.Errorf("gotByPair.ID = %v, want %v", gotByPair.ID, id)
	}
}

func TestStorageImagesGetByIDMissing(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	if _, err := s.Queries().GetStorageImageByID(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetStorageImageByID(unknown) err = %v, want pgx.ErrNoRows", err)
	}
}

// TestStorageImagesCreateUpsertOnConflict verifies the
// ON CONFLICT (template_id, pool_id) DO UPDATE behaviour: a second
// create with the same composite key but different content overwrites
// checksum / size / format on the existing row (id and imported_at
// preserved). Rows track the latest authoritative import.
func TestStorageImagesCreateUpsertOnConflict(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "img-upsert")
	pool := seedPoolForImages(t, ctx, s, node, "img-upsert")

	first, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             uuid.New(),
		TemplateID:     tpl,
		PoolID:         pool,
		ChecksumSha256: imageSHA256(0x01),
		SizeBytes:      1024,
		Format:         "qcow2",
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	second, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             uuid.New(), // ignored on conflict — id stays first.ID
		TemplateID:     tpl,
		PoolID:         pool,
		ChecksumSha256: imageSHA256(0x02),
		SizeBytes:      2048,
		Format:         "raw",
	})
	if err != nil {
		t.Fatalf("second Create (upsert): %v", err)
	}

	if second.ID != first.ID {
		t.Errorf("second.ID = %v, want %v (DO UPDATE preserves id)", second.ID, first.ID)
	}
	if second.ChecksumSha256 != imageSHA256(0x02) {
		t.Errorf("second.ChecksumSha256 = %q, want updated value", second.ChecksumSha256)
	}
	if second.SizeBytes != 2048 {
		t.Errorf("second.SizeBytes = %d, want 2048", second.SizeBytes)
	}
	if second.Format != "raw" {
		t.Errorf("second.Format = %q, want raw", second.Format)
	}
}

// TestStorageImagesUniqueTemplatePoolViolation bypasses
// CreateStorageImage's ON CONFLICT clause via raw SQL to prove the
// underlying UNIQUE (template_id, pool_id) constraint exists. Sanity
// check that the schema invariant is in place at the DB level.
func TestStorageImagesUniqueTemplatePoolViolation(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "img-uniq")
	pool := seedPoolForImages(t, ctx, s, node, "img-uniq")

	if _, err := s.Pool().Exec(ctx,
		`insert into storage_images (template_id, pool_id, checksum_sha256, size_bytes, format)
		 values ($1, $2, $3, 1, 'qcow2')`,
		tpl, pool, imageSHA256(0x10),
	); err != nil {
		t.Fatalf("first raw insert: %v", err)
	}

	_, err := s.Pool().Exec(ctx,
		`insert into storage_images (template_id, pool_id, checksum_sha256, size_bytes, format)
		 values ($1, $2, $3, 1, 'qcow2')`,
		tpl, pool, imageSHA256(0x11),
	)
	if err == nil {
		t.Fatal("second raw insert succeeded; want UNIQUE violation")
	}
	if !strings.Contains(err.Error(), "storage_images_template_id_pool_id_key") &&
		!strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStorageImagesListByPoolPagination(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	pool := seedPoolForImages(t, ctx, s, node, "img-list")

	const total = 5
	ids := make([]uuid.UUID, 0, total)
	importedAt := make([]time.Time, 0, total)
	for i := 0; i < total; i++ {
		tpl := seedTemplateForImages(t, ctx, s, owner, "img-list")
		row, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     tpl,
			PoolID:         pool,
			ChecksumSha256: imageSHA256(byte(i + 1)),
			SizeBytes:      int64(1024 * (i + 1)),
			Format:         "qcow2",
		})
		if err != nil {
			t.Fatalf("Create row %d: %v", i, err)
		}
		ids = append(ids, row.ID)
		importedAt = append(importedAt, row.ImportedAt)
	}

	// Page 1: limit=3, no cursor → newest 3 rows DESC.
	page1, err := s.Queries().ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{
		PoolID:     pool,
		LimitCount: 3,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1))
	}
	// Verify DESC ordering by imported_at then id (the rows were
	// created in chronological order, so newest = ids[total-1]).
	for i := 1; i < len(page1); i++ {
		prev := page1[i-1]
		cur := page1[i]
		if prev.ImportedAt.Before(cur.ImportedAt) {
			t.Errorf("page1 not DESC at %d: %v then %v", i, prev.ImportedAt, cur.ImportedAt)
		}
	}

	// Page 2: cursor advances past last row of page1.
	last := page1[len(page1)-1]
	page2, err := s.Queries().ListStorageImagesByPool(ctx, store.ListStorageImagesByPoolParams{
		PoolID:           pool,
		CursorImportedAt: &last.ImportedAt,
		CursorID:         &last.ID,
		LimitCount:       10,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != total-3 {
		t.Errorf("page2 len = %d, want %d", len(page2), total-3)
	}

	// No row appears twice across pages.
	seen := make(map[uuid.UUID]bool, total)
	for _, r := range page1 {
		seen[r.ID] = true
	}
	for _, r := range page2 {
		if seen[r.ID] {
			t.Errorf("row %v appeared on both pages", r.ID)
		}
	}
	_ = importedAt // kept for future debugging if pagination breaks
	_ = ids
}

func TestCountStorageImagesByTemplate(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "img-cnt-tpl")

	got, err := s.Queries().CountStorageImagesByTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CountStorageImagesByTemplate empty: %v", err)
	}
	if got != 0 {
		t.Errorf("empty count = %d, want 0", got)
	}

	// Create two images for tpl in two different pools.
	for i := 0; i < 2; i++ {
		pool := seedPoolForImages(t, ctx, s, node, "img-cnt-tpl")
		if _, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     tpl,
			PoolID:         pool,
			ChecksumSha256: imageSHA256(byte(i)),
			SizeBytes:      1024,
			Format:         "qcow2",
		}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	got, err = s.Queries().CountStorageImagesByTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CountStorageImagesByTemplate filled: %v", err)
	}
	if got != 2 {
		t.Errorf("filled count = %d, want 2", got)
	}
}

func TestCountStorageImagesByPool(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	pool := seedPoolForImages(t, ctx, s, node, "img-cnt-pool")

	got, err := s.Queries().CountStorageImagesByPool(ctx, pool)
	if err != nil {
		t.Fatalf("CountStorageImagesByPool empty: %v", err)
	}
	if got != 0 {
		t.Errorf("empty count = %d, want 0", got)
	}

	// Three templates each project into the pool.
	for i := 0; i < 3; i++ {
		tpl := seedTemplateForImages(t, ctx, s, owner, "img-cnt-pool")
		if _, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     tpl,
			PoolID:         pool,
			ChecksumSha256: imageSHA256(byte(i)),
			SizeBytes:      1024,
			Format:         "qcow2",
		}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	got, err = s.Queries().CountStorageImagesByPool(ctx, pool)
	if err != nil {
		t.Fatalf("CountStorageImagesByPool filled: %v", err)
	}
	if got != 3 {
		t.Errorf("filled count = %d, want 3", got)
	}
}

// TestCountStorageImagesBySHA256InPoolExcludesSelf is the load-bearing
// test for the refcount gate that sync delete relies on.
// Three rows in one pool share a checksum; excluding any one of them
// returns 2; excluding by a non-matching sha256 returns 0; excluding
// across pools is unaffected (the query is pool-scoped).
func TestCountStorageImagesBySHA256InPoolExcludesSelf(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	poolA := seedPoolForImages(t, ctx, s, node, "img-ref-a")
	poolB := seedPoolForImages(t, ctx, s, node, "img-ref-b")
	sharedSHA := imageSHA256(0xde)
	otherSHA := imageSHA256(0xad)

	// Three rows in poolA share sharedSHA.
	rowsA := make([]uuid.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		tpl := seedTemplateForImages(t, ctx, s, owner, "img-ref-a")
		row, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     tpl,
			PoolID:         poolA,
			ChecksumSha256: sharedSHA,
			SizeBytes:      1024,
			Format:         "qcow2",
		})
		if err != nil {
			t.Fatalf("Create rowA %d: %v", i, err)
		}
		rowsA = append(rowsA, row.ID)
	}

	// One row in poolB shares the same sharedSHA — must NOT count.
	{
		tpl := seedTemplateForImages(t, ctx, s, owner, "img-ref-b")
		if _, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
			ID:             uuid.New(),
			TemplateID:     tpl,
			PoolID:         poolB,
			ChecksumSha256: sharedSHA,
			SizeBytes:      1024,
			Format:         "qcow2",
		}); err != nil {
			t.Fatalf("Create rowB: %v", err)
		}
	}

	// Exclude rowsA[0] → 2 siblings remain in poolA.
	got, err := s.Queries().CountStorageImagesBySHA256InPool(ctx,
		store.CountStorageImagesBySHA256InPoolParams{
			PoolID:         poolA,
			ChecksumSha256: sharedSHA,
			ExcludeID:      rowsA[0],
		})
	if err != nil {
		t.Fatalf("count exclude rowsA[0]: %v", err)
	}
	if got != 2 {
		t.Errorf("count exclude rowsA[0] = %d, want 2", got)
	}

	// Exclude with a non-matching sha256 → 0 (no rows match the
	// checksum filter).
	got, err = s.Queries().CountStorageImagesBySHA256InPool(ctx,
		store.CountStorageImagesBySHA256InPoolParams{
			PoolID:         poolA,
			ChecksumSha256: otherSHA,
			ExcludeID:      rowsA[0],
		})
	if err != nil {
		t.Fatalf("count exclude with otherSHA: %v", err)
	}
	if got != 0 {
		t.Errorf("count exclude with otherSHA = %d, want 0", got)
	}
}

func TestStorageImagesDelete(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	owner := seedUser(t, ctx, s, "developer")
	node := seedNodeForPools(t, ctx, s)
	tpl := seedTemplateForImages(t, ctx, s, owner, "img-del")
	pool := seedPoolForImages(t, ctx, s, node, "img-del")

	id := uuid.New()
	if _, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             id,
		TemplateID:     tpl,
		PoolID:         pool,
		ChecksumSha256: imageSHA256(0x55),
		SizeBytes:      1024,
		Format:         "qcow2",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Queries().DeleteStorageImage(ctx, id); err != nil {
		t.Fatalf("DeleteStorageImage: %v", err)
	}
	if _, err := s.Queries().GetStorageImageByID(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetStorageImageByID after delete err = %v, want pgx.ErrNoRows", err)
	}
}
