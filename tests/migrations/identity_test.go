// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
)

func TestUserEmailReusableAfterSoftDelete(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `insert into users (email, password_hash, role, deleted_at) values ('gone@example.com', 'h', 'developer', now())`); err != nil {
		t.Fatalf("seed soft-deleted user: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `insert into users (email, password_hash, role) values ('gone@example.com', 'h', 'developer')`); err != nil {
		t.Fatalf("expected email reuse after soft-delete to succeed, got: %v", err)
	}
}

func TestUserEmailCaseInsensitiveUnique(t *testing.T) {
	h := shared
	ctx := context.Background()
	if _, err := h.Pool.Exec(ctx, `insert into users (email, password_hash, role) values ('Foo@Example.com', 'h', 'developer')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := h.Pool.Exec(ctx, `insert into users (email, password_hash, role) values ('foo@example.COM', 'h', 'developer')`)
	if !isUnique(err) {
		t.Fatalf("want unique violation on case-insensitive email, got %v", err)
	}
}

func TestApiTokenHashUnique(t *testing.T) {
	h := shared
	ctx := context.Background()
	var userID string
	if err := h.Pool.QueryRow(ctx, `insert into users (email, password_hash, role) values ('t@x.com', 'h', 'developer') returning id`).Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `insert into api_tokens (user_id, name, token_hash, prefix) values ($1, 'a', '\x00112233'::bytea, 'otx_001')`, userID); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := h.Pool.Exec(ctx, `insert into api_tokens (user_id, name, token_hash, prefix) values ($1, 'b', '\x00112233'::bytea, 'otx_002')`, userID)
	if !isUnique(err) {
		t.Fatalf("want unique violation on duplicate token_hash, got %v", err)
	}
}

// Owner RESTRICT semantics: a user that owns any user-resource cannot be
// hard-deleted. Application-level flow must clear ownership first.

func TestUserDeleteRestrictedByOwnedVM(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	_, err := shared.Pool.Exec(ctx, `delete from users where id=$1`, f.ownerID)
	if err == nil {
		t.Fatalf("expected RESTRICT on user delete while a vm references them as owner")
	}
	if !strings.Contains(err.Error(), "23503") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected FK violation 23503, got: %v", err)
	}
}

func TestUserDeleteRestrictedByOwnedTemplate(t *testing.T) {
	ctx := context.Background()
	ownerID := seedUser(t)
	if _, err := shared.Pool.Exec(ctx, `
		insert into templates (owner_id, name, architecture, os_family, image_url, image_checksum_sha256)
		values ($1, 'tpl-restrict-'||substr(uuid_generate_v7()::text,1,8), 'amd64', 'linux', 'https://x', '\x00')`,
		ownerID); err != nil {
		t.Fatalf("template: %v", err)
	}
	_, err := shared.Pool.Exec(ctx, `delete from users where id=$1`, ownerID)
	if err == nil {
		t.Fatalf("expected RESTRICT on user delete while a template references them as owner")
	}
	if !strings.Contains(err.Error(), "23503") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected FK violation 23503, got: %v", err)
	}
}

func TestUserDeleteRestrictedByOwnedSnapshot(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	if _, err := shared.Pool.Exec(ctx, `
		insert into snapshots (vm_id, owner_id, name, status, vm_state_at_snapshot)
		values ($1, $2, 'snap-restrict', 'ready', 'stopped')`, f.vmID, f.ownerID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// f.ownerID also owns the VM, so delete is restricted by both. Make a
	// dedicated snapshot owner to assert snapshot-only restriction.
	snapOwnerID := seedUser(t)
	if _, err := shared.Pool.Exec(ctx, `
		insert into snapshots (vm_id, owner_id, name, status, vm_state_at_snapshot)
		values ($1, $2, 'snap-only-'||substr(uuid_generate_v7()::text,1,8), 'ready', 'stopped')`,
		f.vmID, snapOwnerID); err != nil {
		t.Fatalf("snapshot for dedicated owner: %v", err)
	}
	_, err := shared.Pool.Exec(ctx, `delete from users where id=$1`, snapOwnerID)
	if err == nil {
		t.Fatalf("expected RESTRICT on user delete while a snapshot references them as owner")
	}
	if !strings.Contains(err.Error(), "23503") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected FK violation 23503, got: %v", err)
	}
}
