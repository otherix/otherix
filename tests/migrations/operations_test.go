// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
)

func TestMigrationOnlyOneActivePerVM(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	// Need a second node for the target.
	var node2 string
	if err := shared.Pool.QueryRow(ctx, `
		insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
		values ('hv-target-'||substr(uuid_generate_v7()::text,1,8), 'amd64', 'https://x','x', 49152, 49251) returning id`).Scan(&node2); err != nil {
		t.Fatalf("node2: %v", err)
	}

	if _, err := shared.Pool.Exec(ctx, `
		insert into migrations (vm_id, source_node_id, target_node_id, reason, phase)
		values ($1, $2, $3, 'manual', 'active')`, f.vmID, f.nodeID, node2); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `
		insert into migrations (vm_id, source_node_id, target_node_id, reason, phase)
		values ($1, $2, $3, 'drain', 'setup')`, f.vmID, f.nodeID, node2); !isUnique(err) {
		t.Fatalf("want unique violation on second active migration, got %v", err)
	}

	// Move first to terminal — second should now be allowed.
	if _, err := shared.Pool.Exec(ctx, `update migrations set phase='completed', completed_at=now() where vm_id=$1`, f.vmID); err != nil {
		t.Fatalf("update to terminal: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `
		insert into migrations (vm_id, source_node_id, target_node_id, reason, phase)
		values ($1, $2, $3, 'drain', 'setup')`, f.vmID, f.nodeID, node2); err != nil {
		t.Fatalf("second after terminal: %v", err)
	}
}

func TestMigrationDistinctNodesCheck(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	if _, err := shared.Pool.Exec(ctx, `
		insert into migrations (vm_id, source_node_id, target_node_id, reason)
		values ($1, $2, $2, 'manual')`, f.vmID, f.nodeID); err == nil {
		t.Fatalf("expected CHECK violation: source and target equal")
	}
}

func TestIdempotencyKeyUnique(t *testing.T) {
	ctx := context.Background()
	suffix := randSlug(t)
	key := "k-" + suffix
	if _, err := shared.Pool.Exec(ctx, `
		insert into idempotency_keys (key, request_method, request_path, request_hash, expires_at)
		values ($1, 'POST', '/v1/vms', '\x00', now()+interval '1 hour')`, key); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Same key again — must violate the primary key.
	_, err := shared.Pool.Exec(ctx, `
		insert into idempotency_keys (key, request_method, request_path, request_hash, expires_at)
		values ($1, 'POST', '/v1/vms', '\x01', now()+interval '1 hour')`, key)
	if !isUnique(err) {
		t.Fatalf("want unique violation on duplicate key, got %v", err)
	}
}

func TestAuditLogInsertSurvivesUnknownActor(t *testing.T) {
	ctx := context.Background()
	// actor_user_id points to a non-existent user — must succeed (no FK by design).
	bogus := "00000000-0000-0000-0000-0000deadbeef"
	if _, err := shared.Pool.Exec(ctx, `
		insert into audit_log (actor_user_id, actor_kind, action, resource_type, created_at)
		values ($1, 'user', 'vm.create', 'vm', now())`, bogus); err != nil {
		t.Fatalf("audit insert with bogus actor (no-FK design): %v", err)
	}
}

func TestAuditLogPartitionsExist(t *testing.T) {
	ctx := context.Background()
	rows, err := shared.Pool.Query(ctx, `
		select tablename from pg_tables
		where schemaname='public' and tablename like 'audit_log_y%_m%'
		order by tablename`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var partitions []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		partitions = append(partitions, s)
	}
	if len(partitions) < 3 {
		t.Fatalf("expected ≥3 audit_log partitions seeded, got %d: %v", len(partitions), partitions)
	}
}

func TestAuditLogDefaultPartitionAcceptsOutOfRangeRow(t *testing.T) {
	ctx := context.Background()
	// A timestamp far beyond the seeded monthly partitions (2026-05..2026-07)
	// must still INSERT cleanly, landing in audit_log_default.
	if _, err := shared.Pool.Exec(ctx, `
		insert into audit_log (actor_kind, action, resource_type, created_at)
		values ('system', 'cleanup.test', 'audit_log', '2030-01-15 00:00:00+00')`); err != nil {
		t.Fatalf("default partition should accept far-future row: %v", err)
	}
	// Confirm it actually landed in the default partition.
	var n int
	if err := shared.Pool.QueryRow(ctx, `
		select count(*) from audit_log_default
		where action='cleanup.test' and created_at='2030-01-15 00:00:00+00'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row in audit_log_default, got %d", n)
	}
}
