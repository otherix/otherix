// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
)

func TestNodeNameUniqueAmongLive(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `
		insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
		values ('hv-1', 'amd64', 'https://10.0.0.1:9443', '10.0.0.1', 49152, 49251)`); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := h.Pool.Exec(ctx, `
		insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
		values ('hv-1', 'amd64', 'https://10.0.0.2:9443', '10.0.0.2', 49152, 49251)`)
	if !isUnique(err) {
		t.Fatalf("want unique violation on duplicate node name, got %v", err)
	}
}

func TestAgentCertCascadesWithNode(t *testing.T) {
	h := shared
	ctx := context.Background()
	var nodeID string
	if err := h.Pool.QueryRow(ctx, `
		insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
		values ('hv-2', 'amd64', 'https://10.0.0.3:9443', '10.0.0.3', 49152, 49251)
		returning id`).Scan(&nodeID); err != nil {
		t.Fatalf("node: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `
		insert into agent_certs (node_id, serial, fingerprint_sha256, subject_dn, not_before, not_after)
		values ($1, '\x01', '\x02', 'CN=hv-2', now(), now()+interval '1 year')`, nodeID); err != nil {
		t.Fatalf("cert: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `delete from nodes where id=$1`, nodeID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := h.Pool.QueryRow(ctx, `select count(*) from agent_certs where node_id=$1`, nodeID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("agent_certs not cascaded, n=%d", n)
	}
}

// TestJoinTokenConsumptionNodeFKSetNull verifies the audit-table FK
// semantics: deleting a node nulls the consumed_by_node_id pointer in
// join_token_consumptions, preserving the audit row but losing the
// link to the (now-gone) node.
func TestJoinTokenConsumptionNodeFKSetNull(t *testing.T) {
	h := shared
	ctx := context.Background()
	var nodeID, tokenID string
	if err := h.Pool.QueryRow(ctx, `
		insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
		values ('hv-3', 'arm64', 'https://10.0.0.4:9443', '10.0.0.4', 49152, 49251)
		returning id`).Scan(&nodeID); err != nil {
		t.Fatalf("node: %v", err)
	}
	if err := h.Pool.QueryRow(ctx, `
		insert into join_tokens (token_hash, expires_at)
		values ('\x99'::bytea, now()+interval '1 hour')
		returning id`).Scan(&tokenID); err != nil {
		t.Fatalf("token: %v", err)
	}
	var consumptionID string
	if err := h.Pool.QueryRow(ctx, `
		insert into join_token_consumptions (join_token_id, consumed_by_node_id)
		values ($1, $2)
		returning id`, tokenID, nodeID).Scan(&consumptionID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `delete from nodes where id=$1`, nodeID); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	var consumedBy *string
	if err := h.Pool.QueryRow(ctx,
		`select consumed_by_node_id from join_token_consumptions where id=$1`,
		consumptionID).Scan(&consumedBy); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if consumedBy != nil {
		t.Fatalf("want nulled consumed_by_node_id, got %v", *consumedBy)
	}
}

// TestJoinTokenConsumptionCascadeOnTokenDelete verifies the
// join_token_id FK ON DELETE CASCADE: removing the parent token
// drops every consumption row in lock-step.
func TestJoinTokenConsumptionCascadeOnTokenDelete(t *testing.T) {
	h := shared
	ctx := context.Background()
	var nodeID, tokenID string
	if err := h.Pool.QueryRow(ctx, `
		insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
		values ('hv-cascade', 'amd64', 'https://10.0.0.5:9443', '10.0.0.5', 49152, 49251)
		returning id`).Scan(&nodeID); err != nil {
		t.Fatalf("node: %v", err)
	}
	if err := h.Pool.QueryRow(ctx, `
		insert into join_tokens (token_hash, expires_at)
		values ('\xaa'::bytea, now()+interval '1 hour')
		returning id`).Scan(&tokenID); err != nil {
		t.Fatalf("token: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `
		insert into join_token_consumptions (join_token_id, consumed_by_node_id)
		values ($1, $2)`, tokenID, nodeID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `delete from join_tokens where id=$1`, tokenID); err != nil {
		t.Fatalf("delete token: %v", err)
	}
	var count int
	if err := h.Pool.QueryRow(ctx,
		`select count(*) from join_token_consumptions where join_token_id=$1`,
		tokenID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("CASCADE failed: consumption count = %d, want 0", count)
	}
}

// TestJoinTokenPreboundMultiUseCheckRejects verifies the SQL CHECK
// constraint chk_join_tokens_prebound_single_use forbids
// intended_node_name set + max_uses > 1 (defense-in-depth against
// API-edge regressions).
func TestJoinTokenPreboundMultiUseCheckRejects(t *testing.T) {
	h := shared
	ctx := context.Background()
	_, err := h.Pool.Exec(ctx, `
		insert into join_tokens (token_hash, expires_at, intended_node_name, max_uses)
		values ('\xbb'::bytea, now()+interval '1 hour', 'node-multiuse', 5)`)
	if err == nil {
		t.Fatal("expected CHECK constraint violation, got nil")
	}
}

// TestCAActiveAtMostOne verifies the partial unique index
// uq_ca_certs_active enforces at-most-one-active. Required by the
// race-safety guarantee for concurrent CA bootstrap.
func TestCAActiveAtMostOne(t *testing.T) {
	h := shared
	ctx := context.Background()
	// Wipe any inherited CA rows (other tests may have generated one).
	if _, err := h.Pool.Exec(ctx, `delete from ca_certs`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	_, err := h.Pool.Exec(ctx, `
		insert into ca_certs (cert_pem, key_pem, fingerprint_sha256, not_before, not_after)
		values ('\x01'::bytea, '\x02'::bytea, '\x03'::bytea, now(), now()+interval '1 year')`)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = h.Pool.Exec(ctx, `
		insert into ca_certs (cert_pem, key_pem, fingerprint_sha256, not_before, not_after)
		values ('\x04'::bytea, '\x05'::bytea, '\x06'::bytea, now(), now()+interval '1 year')`)
	if err == nil {
		t.Fatal("expected unique violation on second active row, got nil")
	}
}

func TestFirmwareDefaultIsUnique(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `
		insert into firmwares (name, architecture, type, is_default)
		values ('OVMF-amd64-default', 'amd64', 'uefi', true)`); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := h.Pool.Exec(ctx, `
		insert into firmwares (name, architecture, type, is_default)
		values ('OVMF-amd64-secboot', 'amd64', 'uefi', true)`)
	if !isUnique(err) {
		t.Fatalf("want unique violation on second default for (amd64, uefi), got %v", err)
	}
}

func TestStoragePoolSameNameAcrossNodesAllowed(t *testing.T) {
	// Multi-instance invariant: pool name is per-node UNIQUE
	// (uq_storage_pools_name on (node_id, lower(name))). The same
	// conceptual pool may exist on multiple nodes as separate
	// instances; this test pins the invariant.
	h := shared
	ctx := context.Background()
	insertNode := func(name, host string, portStart, portEnd int32) string {
		var id string
		if err := h.Pool.QueryRow(ctx, `
			insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end)
			values ($1, 'amd64', $2, $3, $4, $5)
			returning id`, name, "https://"+host+":9443", host, portStart, portEnd).Scan(&id); err != nil {
			t.Fatalf("node %s: %v", name, err)
		}
		return id
	}
	nodeA := insertNode("hv-sp-a", "10.0.0.5", 49152, 49251)
	nodeB := insertNode("hv-sp-b", "10.0.0.6", 49152, 49251)

	if _, err := h.Pool.Exec(ctx, `
		insert into storage_pools (node_id, name, type, path)
		values ($1, 'default', 'local_dir', '/opt/otherix/pools/default')`, nodeA); err != nil {
		t.Fatalf("first instance: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `
		insert into storage_pools (node_id, name, type, path)
		values ($1, 'default', 'local_dir', '/opt/otherix/pools/default')`, nodeB); err != nil {
		t.Fatalf("second instance on different node should be allowed, got: %v", err)
	}

	// Same name on the same node must still fail.
	_, err := h.Pool.Exec(ctx, `
		insert into storage_pools (node_id, name, type, path)
		values ($1, 'default', 'local_dir', '/opt/otherix/pools/default-2')`, nodeA)
	if !isUnique(err) {
		t.Fatalf("want unique violation on duplicate per-node name, got %v", err)
	}
}

func TestClusterSettingsSingleton(t *testing.T) {
	// cluster_settings holds a single row pinned to id=1 by check
	// constraint. Attempting to insert another row must fail the
	// check; the singleton itself was seeded by the migration.
	h := shared
	ctx := context.Background()
	var count int
	if err := h.Pool.QueryRow(ctx, `select count(*) from cluster_settings`).Scan(&count); err != nil {
		t.Fatalf("count cluster_settings: %v", err)
	}
	if count != 1 {
		t.Fatalf("cluster_settings row count = %d, want 1 (migration seed)", count)
	}
	if _, err := h.Pool.Exec(ctx, `insert into cluster_settings (id) values (2)`); err == nil {
		t.Fatal("want check-constraint violation on id != 1, got nil")
	}
}

func TestNetworkVlanCheck(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `
		insert into networks (name, type, bridge_name, vlan_tag)
		values ('bad', 'bridge', 'br0', 5000)`); err == nil {
		t.Fatalf("want CHECK violation for vlan_tag=5000")
	}
	if _, err := h.Pool.Exec(ctx, `
		insert into networks (name, type, bridge_name, vlan_tag)
		values ('ok', 'bridge', 'br0', 100)`); err != nil {
		t.Fatalf("vlan_tag=100 should be valid: %v", err)
	}
}
