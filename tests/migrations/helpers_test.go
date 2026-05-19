// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUUIDGenerateV7_VersionAndVariant(t *testing.T) {
	h := shared
	ctx := context.Background()
	var s string
	if err := h.Pool.QueryRow(ctx, `select uuid_generate_v7()`).Scan(&s); err != nil {
		t.Fatalf("query: %v", err)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id.Version() != 7 {
		t.Fatalf("want version 7, got %d", id.Version())
	}
	if id.Variant() != uuid.RFC4122 {
		t.Fatalf("want RFC4122 variant, got %v", id.Variant())
	}
}

func TestUUIDGenerateV7_TimestampNonDecreasing(t *testing.T) {
	h := shared
	ctx := context.Background()
	const N = 200
	rows, err := h.Pool.Query(ctx, `select uuid_generate_v7() from generate_series(1, $1)`, N)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, N)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		id, err := uuid.Parse(s)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(ids) != N {
		t.Fatalf("got %d uuids, want %d", len(ids), N)
	}
	// Bytes 0..5 carry the 48-bit unix_ts_ms — validate only that the timestamp
	// portion is non-decreasing. Within the same millisecond bytes 6–15 are
	// random, so full byte-level sort order is NOT guaranteed within a ms.
	for i := 1; i < len(ids); i++ {
		prev := tsFromV7(ids[i-1])
		cur := tsFromV7(ids[i])
		if cur < prev {
			t.Fatalf("uuid v7 timestamps regressed at index %d: prev=%d cur=%d", i, prev, cur)
		}
	}
	// Last timestamp should be reasonably close to wall clock.
	now := time.Now().UnixMilli()
	last := tsFromV7(ids[len(ids)-1])
	if last > now+5_000 || last < now-5_000 {
		t.Fatalf("uuid v7 ts %d far from wall clock %d", last, now)
	}
}

// tsFromV7 reads the 48-bit unix_ts_ms from bytes 0..5 of a v7 UUID.
func tsFromV7(id uuid.UUID) int64 {
	b := id[:]
	return int64(b[0])<<40 | int64(b[1])<<32 | int64(b[2])<<24 |
		int64(b[3])<<16 | int64(b[4])<<8 | int64(b[5])
}

func TestSetUpdatedAtTrigger(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `
		create table tmp_t (
			id uuid primary key default uuid_generate_v7(),
			payload text,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);
		create trigger tmp_t_updated before update on tmp_t
			for each row execute function trg_set_updated_at();
	`); err != nil {
		t.Fatalf("create temp table: %v", err)
	}

	var id string
	if err := h.Pool.QueryRow(ctx, `insert into tmp_t (payload) values ('a') returning id`).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var before, after time.Time
	if err := h.Pool.QueryRow(ctx, `select updated_at from tmp_t where id=$1`, id).Scan(&before); err != nil {
		t.Fatalf("select before: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := h.Pool.Exec(ctx, `update tmp_t set payload='b' where id=$1`, id); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := h.Pool.QueryRow(ctx, `select updated_at from tmp_t where id=$1`, id).Scan(&after); err != nil {
		t.Fatalf("select after: %v", err)
	}
	if !after.After(before) {
		t.Fatalf("updated_at did not advance: before=%v after=%v", before, after)
	}
}

func TestBumpGenerationTrigger(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `
		create table tmp_g (
			id uuid primary key default uuid_generate_v7(),
			watched text,
			cosmetic text,
			generation bigint not null default 1,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);
		create trigger tmp_g_bump before update on tmp_g
			for each row execute function trg_bump_generation('watched');
		create trigger tmp_g_updated before update on tmp_g
			for each row execute function trg_set_updated_at();
	`); err != nil {
		t.Fatalf("create: %v", err)
	}

	var id string
	if err := h.Pool.QueryRow(ctx, `insert into tmp_g (watched, cosmetic) values ('a','x') returning id`).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// cosmetic-only update should NOT bump generation
	if _, err := h.Pool.Exec(ctx, `update tmp_g set cosmetic='y' where id=$1`, id); err != nil {
		t.Fatalf("cosmetic update: %v", err)
	}
	var g int64
	if err := h.Pool.QueryRow(ctx, `select generation from tmp_g where id=$1`, id).Scan(&g); err != nil {
		t.Fatalf("select gen: %v", err)
	}
	if g != 1 {
		t.Fatalf("after cosmetic update want generation=1, got %d", g)
	}

	// watched update SHOULD bump generation
	if _, err := h.Pool.Exec(ctx, `update tmp_g set watched='b' where id=$1`, id); err != nil {
		t.Fatalf("watched update: %v", err)
	}
	if err := h.Pool.QueryRow(ctx, `select generation from tmp_g where id=$1`, id).Scan(&g); err != nil {
		t.Fatalf("select gen: %v", err)
	}
	if g != 2 {
		t.Fatalf("after watched update want generation=2, got %d", g)
	}
}

func TestBumpGenerationTrigger_Nulls(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `
		create table tmp_g_nulls (
			id uuid primary key default uuid_generate_v7(),
			watched text,
			generation bigint not null default 1,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);
		create trigger tmp_g_nulls_bump before update on tmp_g_nulls
			for each row execute function trg_bump_generation('watched');
	`); err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1) NULL → NULL: must NOT bump.
	var id string
	if err := h.Pool.QueryRow(ctx, `insert into tmp_g_nulls (watched) values (null) returning id`).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `update tmp_g_nulls set watched = null where id=$1`, id); err != nil {
		t.Fatalf("null→null update: %v", err)
	}
	var g int64
	if err := h.Pool.QueryRow(ctx, `select generation from tmp_g_nulls where id=$1`, id).Scan(&g); err != nil {
		t.Fatalf("scan gen (NULL→NULL): %v", err)
	}
	if g != 1 {
		t.Fatalf("NULL→NULL: want generation=1, got %d", g)
	}

	// 2) NULL → 'a': must bump.
	if _, err := h.Pool.Exec(ctx, `update tmp_g_nulls set watched = 'a' where id=$1`, id); err != nil {
		t.Fatalf("null→a update: %v", err)
	}
	if err := h.Pool.QueryRow(ctx, `select generation from tmp_g_nulls where id=$1`, id).Scan(&g); err != nil {
		t.Fatalf("scan gen (NULL→'a'): %v", err)
	}
	if g != 2 {
		t.Fatalf("NULL→'a': want generation=2, got %d", g)
	}

	// 3) 'a' → NULL: must bump.
	if _, err := h.Pool.Exec(ctx, `update tmp_g_nulls set watched = null where id=$1`, id); err != nil {
		t.Fatalf("a→null update: %v", err)
	}
	if err := h.Pool.QueryRow(ctx, `select generation from tmp_g_nulls where id=$1`, id).Scan(&g); err != nil {
		t.Fatalf("scan gen ('a'→NULL): %v", err)
	}
	if g != 3 {
		t.Fatalf("'a'→NULL: want generation=3, got %d", g)
	}
}
