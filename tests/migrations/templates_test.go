// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
)

func TestTemplateNameUnique(t *testing.T) {
	h := shared
	ctx := context.Background()
	ownerID := seedUser(t)
	if _, err := h.Pool.Exec(ctx, `
		insert into templates (owner_id, name, architecture, os_family, image_url, image_checksum_sha256)
		values ($1, 'ubuntu-24.04', 'amd64', 'linux', 'https://x/u.qcow2', '\x00')`, ownerID); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := h.Pool.Exec(ctx, `
		insert into templates (owner_id, name, architecture, os_family, image_url, image_checksum_sha256)
		values ($1, 'ubuntu-24.04', 'amd64', 'linux', 'https://y/u.qcow2', '\x00')`, ownerID); !isUnique(err) {
		t.Fatalf("want unique violation on second template with same name, got %v", err)
	}
}

func TestTemplateCpuMemoryDiskCheck(t *testing.T) {
	h := shared
	ctx := context.Background()
	ownerID := seedUser(t)
	if _, err := h.Pool.Exec(ctx, `
		insert into templates (owner_id, name, architecture, os_family, image_url, image_checksum_sha256, default_cpu_cores)
		values ($1, 'bad-cpu', 'amd64', 'linux', 'https://x', '\x00', 0)`, ownerID); err == nil {
		t.Fatalf("expected CHECK violation on default_cpu_cores=0")
	}
}

// TestNodeImageCacheCompositePK was removed along with the
// node_image_cache table itself (replaced by the storage_images
// junction). Vertical-slice tests cover the new junction's
// invariants.
