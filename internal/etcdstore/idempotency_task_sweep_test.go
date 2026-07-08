// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

// seedIdempotencyTaskIndex writes an index entry with a chosen ExpiresAt so the
// sweep can be driven against a known keyspace.
func seedIdempotencyTaskIndex(t *testing.T, cli *etcd.Client, u uuid.UUID, key string, taskID uuid.UUID, expiresAt time.Time) {
	t.Helper()
	b, err := marshalIdempotencyTaskIndex(idempotencyTaskIndex{
		TaskID:      taskID,
		RequestHash: []byte("h"),
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		t.Fatalf("marshalIdempotencyTaskIndex: %v", err)
	}
	if _, err := cli.Raw().Put(context.Background(), idempotencyTaskIndexKey(u, key), string(b)); err != nil {
		t.Fatalf("seed index %q: %v", key, err)
	}
}

func idempotencyTaskIndexExists(t *testing.T, cli *etcd.Client, key string) bool {
	t.Helper()
	resp, err := cli.Raw().Get(context.Background(), key, clientv3.WithCountOnly())
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	return resp.Count > 0
}

func TestDeleteExpiredIdempotencyTaskIndex(t *testing.T) {
	st, cli := FreshStore(t)
	ctx := context.Background()
	u := uuid.New()
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	seedIdempotencyTaskIndex(t, cli, u, "old", uuid.New(), past)
	seedIdempotencyTaskIndex(t, cli, u, "new", uuid.New(), future)

	n, err := st.DeleteExpiredIdempotencyTaskIndex(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteExpiredIdempotencyTaskIndex deleted = %d, want 1", n)
	}
	if idempotencyTaskIndexExists(t, cli, idempotencyTaskIndexKey(u, "old")) {
		t.Errorf("expired index survived the sweep")
	}
	if !idempotencyTaskIndexExists(t, cli, idempotencyTaskIndexKey(u, "new")) {
		t.Errorf("live index was wrongly swept")
	}
}
