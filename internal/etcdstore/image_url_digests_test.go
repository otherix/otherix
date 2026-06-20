//go:build integration

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
)

func TestImageURLDigestLWW(t *testing.T) {
	st, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	const url = "https://example.test/noble-cloudimg-arm64.img"
	node := uuid.New()
	t0 := time.Now().UTC().Truncate(time.Second)

	if _, ok, err := st.ImageURLDigest(ctx, url); err != nil || ok {
		t.Fatalf("ImageURLDigest before write = ok %v, err %v; want ok=false, err=nil", ok, err)
	}
	if err := st.UpsertImageURLDigest(ctx, url, "aaaa", 100, t0, node); err != nil {
		t.Fatalf("upsert d1: %v", err)
	}
	if d, ok, err := st.ImageURLDigest(ctx, url); err != nil || !ok || d != "aaaa" {
		t.Fatalf("after d1 = %q ok %v err %v; want aaaa", d, ok, err)
	}
	if err := st.UpsertImageURLDigest(ctx, url, "bbbb", 100, t0.Add(time.Minute), node); err != nil {
		t.Fatalf("upsert d2: %v", err)
	}
	if d, _, _ := st.ImageURLDigest(ctx, url); d != "bbbb" {
		t.Fatalf("after d2 = %q; want bbbb (newer wins)", d)
	}
	if err := st.UpsertImageURLDigest(ctx, url, "cccc", 100, t0, node); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	if d, _, _ := st.ImageURLDigest(ctx, url); d != "bbbb" {
		t.Fatalf("after stale = %q; want bbbb (older ignored)", d)
	}
}
