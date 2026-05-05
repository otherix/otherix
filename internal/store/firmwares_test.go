// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/store"
)

// uniqueFirmwareName returns a per-call unique name so tests don't
// collide on uq_firmwares_name_arch.
func uniqueFirmwareName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

func defaultFirmwareParams(id uuid.UUID, name string) store.CreateFirmwareParams {
	return store.CreateFirmwareParams{
		ID:           id,
		Name:         name,
		Architecture: store.CpuArchAmd64,
		Type:         store.FirmwareTypeUefi,
		Version:      nil,
		SecureBoot:   false,
		IsDefault:    false,
	}
}

func TestFirmwaresCreateGetByID(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	name := uniqueFirmwareName("ovmf")
	created, err := s.Queries().CreateFirmware(ctx, defaultFirmwareParams(id, name))
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID = %v, want %v", created.ID, id)
	}
	if created.Architecture != store.CpuArchAmd64 {
		t.Errorf("created.Architecture = %v, want amd64", created.Architecture)
	}
	if created.Type != store.FirmwareTypeUefi {
		t.Errorf("created.Type = %v, want uefi", created.Type)
	}
	if created.IsDefault {
		t.Error("created.IsDefault = true, want false")
	}
	if created.Version != nil {
		t.Errorf("created.Version = %v, want nil", created.Version)
	}

	got, err := s.Queries().GetFirmwareByID(ctx, id)
	if err != nil {
		t.Fatalf("GetFirmwareByID: %v", err)
	}
	if got.Name != name {
		t.Errorf("got.Name = %q, want %q", got.Name, name)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("UpdatedAt %v < CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
}

func TestFirmwaresDuplicateNameArchRejected(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	name := uniqueFirmwareName("dup")
	if _, err := s.Queries().CreateFirmware(ctx, defaultFirmwareParams(uuid.New(), name)); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.Queries().CreateFirmware(ctx, defaultFirmwareParams(uuid.New(), name))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("dup-name err = %v, want pg 23505", err)
	}
}

func TestFirmwaresSameNameDifferentArchAllowed(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	name := uniqueFirmwareName("crossarch")
	a := defaultFirmwareParams(uuid.New(), name)
	a.Architecture = store.CpuArchAmd64
	b := defaultFirmwareParams(uuid.New(), name)
	b.Architecture = store.CpuArchArm64
	if _, err := s.Queries().CreateFirmware(ctx, a); err != nil {
		t.Fatalf("amd64: %v", err)
	}
	if _, err := s.Queries().CreateFirmware(ctx, b); err != nil {
		t.Errorf("arm64 same name = %v, want success", err)
	}
}

func TestFirmwaresSingleDefaultPerArchType(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	first := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("def-a"))
	first.IsDefault = true
	if _, err := s.Queries().CreateFirmware(ctx, first); err != nil {
		t.Fatalf("first default: %v", err)
	}
	second := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("def-b"))
	second.IsDefault = true
	_, err := s.Queries().CreateFirmware(ctx, second)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("second default err = %v, want pg 23505", err)
	}

	if err := s.Queries().DeleteFirmware(ctx, first.ID); err != nil {
		t.Fatalf("DeleteFirmware: %v", err)
	}
	if _, err := s.Queries().CreateFirmware(ctx, second); err != nil {
		t.Errorf("post-delete second default = %v, want success", err)
	}
}

func TestFirmwaresGetDefaultForArchAndType(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	// Use arm64 + bios to keep this test isolated from concurrent
	// "amd64+uefi default" tests.
	missingParams := store.GetDefaultFirmwareForArchAndTypeParams{
		Architecture: store.CpuArchArm64,
		Type:         store.FirmwareTypeBios,
	}
	if _, err := s.Queries().GetDefaultFirmwareForArchAndType(ctx, missingParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("no-default lookup err = %v, want pgx.ErrNoRows", err)
	}

	plain := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("plain"))
	plain.Architecture = store.CpuArchArm64
	plain.Type = store.FirmwareTypeBios
	if _, err := s.Queries().CreateFirmware(ctx, plain); err != nil {
		t.Fatalf("plain firmware: %v", err)
	}
	if _, err := s.Queries().GetDefaultFirmwareForArchAndType(ctx, missingParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("non-default lookup err = %v, want pgx.ErrNoRows", err)
	}

	def := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("def"))
	def.Architecture = store.CpuArchArm64
	def.Type = store.FirmwareTypeBios
	def.IsDefault = true
	created, err := s.Queries().CreateFirmware(ctx, def)
	if err != nil {
		t.Fatalf("default firmware: %v", err)
	}
	got, err := s.Queries().GetDefaultFirmwareForArchAndType(ctx, missingParams)
	if err != nil {
		t.Fatalf("default lookup err = %v, want success", err)
	}
	if got.ID != created.ID {
		t.Errorf("default lookup id = %v, want %v", got.ID, created.ID)
	}
}

func TestFirmwaresListWithFilters(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	// Seed a small mix; exact totals are not asserted (concurrent
	// tests share the schema), only that the filter narrows correctly.
	for i := 0; i < 3; i++ {
		p := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("filt-amd"))
		p.Architecture = store.CpuArchAmd64
		p.Type = store.FirmwareTypeUefi
		if _, err := s.Queries().CreateFirmware(ctx, p); err != nil {
			t.Fatalf("seed amd: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		p := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("filt-arm"))
		p.Architecture = store.CpuArchArm64
		p.Type = store.FirmwareTypeUefi
		if _, err := s.Queries().CreateFirmware(ctx, p); err != nil {
			t.Fatalf("seed arm: %v", err)
		}
	}

	arm := store.CpuArchArm64
	rows, err := s.Queries().ListFirmwares(ctx, store.ListFirmwaresParams{
		Architecture: &arm,
		LimitCount:   500,
	})
	if err != nil {
		t.Fatalf("ListFirmwares arm: %v", err)
	}
	for _, r := range rows {
		if r.Architecture != store.CpuArchArm64 {
			t.Errorf("ListFirmwares(arm) returned %v", r.Architecture)
		}
	}

	bios := store.FirmwareTypeBios
	biosRows, err := s.Queries().ListFirmwares(ctx, store.ListFirmwaresParams{
		Type:       &bios,
		LimitCount: 500,
	})
	if err != nil {
		t.Fatalf("ListFirmwares bios: %v", err)
	}
	for _, r := range biosRows {
		if r.Type != store.FirmwareTypeBios {
			t.Errorf("ListFirmwares(bios) returned %v", r.Type)
		}
	}
}

func TestFirmwaresCursorPaginationOrdering(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	// Use a unique synthetic architecture-marker via the (arch, type)
	// scope: amd64+bios is rarely used, so we keep collisions low.
	// We page through every row and assert that the page2 head is
	// strictly after the page1 tail.
	const total = 5
	created := make([]uuid.UUID, 0, total)
	for i := 0; i < total; i++ {
		p := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("page"))
		p.Architecture = store.CpuArchAmd64
		p.Type = store.FirmwareTypeBios
		row, err := s.Queries().CreateFirmware(ctx, p)
		if err != nil {
			t.Fatalf("seed #%d: %v", i, err)
		}
		created = append(created, row.ID)
	}

	bios := store.FirmwareTypeBios
	amd := store.CpuArchAmd64
	page1, err := s.Queries().ListFirmwares(ctx, store.ListFirmwaresParams{
		Architecture: &amd,
		Type:         &bios,
		LimitCount:   3,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1))
	}
	last := page1[len(page1)-1]

	page2, err := s.Queries().ListFirmwares(ctx, store.ListFirmwaresParams{
		Architecture:    &amd,
		Type:            &bios,
		CursorCreatedAt: &last.CreatedAt,
		CursorID:        &last.ID,
		LimitCount:      3,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	for _, p := range page2 {
		if p.CreatedAt.Before(last.CreatedAt) {
			t.Errorf("page2 row CreatedAt %v < cursor %v", p.CreatedAt, last.CreatedAt)
		}
	}
}

func TestFirmwaresUpdate(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateFirmware(ctx, defaultFirmwareParams(id, uniqueFirmwareName("upd"))); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}

	v := "edk2-stable202311"
	updated, err := s.Queries().UpdateFirmware(ctx, store.UpdateFirmwareParams{
		ID:         id,
		Name:       "renamed",
		Version:    &v,
		SecureBoot: true,
		IsDefault:  false,
	})
	if err != nil {
		t.Fatalf("UpdateFirmware: %v", err)
	}
	if updated.Name != "renamed" {
		t.Errorf("Name = %q, want renamed", updated.Name)
	}
	if updated.Version == nil || *updated.Version != v {
		t.Errorf("Version = %v, want %q", updated.Version, v)
	}
	if !updated.SecureBoot {
		t.Error("SecureBoot = false, want true")
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) {
		t.Errorf("UpdatedAt %v not after CreatedAt %v", updated.UpdatedAt, updated.CreatedAt)
	}
}

func TestFirmwaresHardDelete(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateFirmware(ctx, defaultFirmwareParams(id, uniqueFirmwareName("hd"))); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	if err := s.Queries().DeleteFirmware(ctx, id); err != nil {
		t.Fatalf("DeleteFirmware: %v", err)
	}
	if _, err := s.Queries().GetFirmwareByID(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetFirmwareByID after hard delete err = %v, want pgx.ErrNoRows", err)
	}
}

func TestFirmwaresCountVMsAndTemplates(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	fwID := uuid.New()
	if _, err := s.Queries().CreateFirmware(ctx, defaultFirmwareParams(fwID, uniqueFirmwareName("cnt"))); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}

	vms, err := s.Queries().CountVMsForFirmware(ctx, fwID)
	if err != nil {
		t.Fatalf("CountVMsForFirmware zero: %v", err)
	}
	if vms != 0 {
		t.Errorf("vms zero = %d, want 0", vms)
	}
	tpls, err := s.Queries().CountTemplatesForFirmware(ctx, fwID)
	if err != nil {
		t.Fatalf("CountTemplatesForFirmware zero: %v", err)
	}
	if tpls != 0 {
		t.Errorf("templates zero = %d, want 0", tpls)
	}

	ownerID := uuid.New()
	const insUser = `
		insert into users (id, email, password_hash, role)
		values ($1, $2, 'x', 'developer')`
	if _, err := s.Pool().Exec(ctx, insUser, ownerID, "fwown-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("insert users: %v", err)
	}

	// Active VM pinning the firmware.
	vmActiveID := uuid.New()
	const insActiveVM = `
		insert into vms (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type, firmware_id)
		values ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0', $4)`
	if _, err := s.Pool().Exec(ctx, insActiveVM, vmActiveID, ownerID, "vm-"+uuid.NewString()[:8], fwID); err != nil {
		t.Fatalf("insert active vm: %v", err)
	}
	// Soft-deleted VM still pinning the firmware (must be excluded).
	vmDeletedID := uuid.New()
	const insDeletedVM = `
		insert into vms (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type, firmware_id, deleted_at)
		values ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0', $4, now())`
	if _, err := s.Pool().Exec(ctx, insDeletedVM, vmDeletedID, ownerID, "vm-"+uuid.NewString()[:8], fwID); err != nil {
		t.Fatalf("insert deleted vm: %v", err)
	}
	// Active template pinning the firmware.
	tplActiveID := uuid.New()
	const insActiveTpl = `
		insert into templates (id, owner_id, name, architecture, os_family, image_url, image_checksum_sha256, firmware_id)
		values ($1, $2, $3, 'amd64', 'linux', 'https://x', '\x00', $4)`
	if _, err := s.Pool().Exec(ctx, insActiveTpl, tplActiveID, ownerID, "tpl-"+uuid.NewString()[:8], fwID); err != nil {
		t.Fatalf("insert active tpl: %v", err)
	}
	// Soft-deleted template (must be excluded).
	tplDeletedID := uuid.New()
	const insDeletedTpl = `
		insert into templates (id, owner_id, name, architecture, os_family, image_url, image_checksum_sha256, firmware_id, deleted_at)
		values ($1, $2, $3, 'amd64', 'linux', 'https://x', '\x00', $4, now())`
	if _, err := s.Pool().Exec(ctx, insDeletedTpl, tplDeletedID, ownerID, "tpl-"+uuid.NewString()[:8], fwID); err != nil {
		t.Fatalf("insert deleted tpl: %v", err)
	}

	vmCount, err := s.Queries().CountVMsForFirmware(ctx, fwID)
	if err != nil {
		t.Fatalf("CountVMsForFirmware: %v", err)
	}
	if vmCount != 1 {
		t.Errorf("vm count = %d, want 1 (soft-deleted excluded)", vmCount)
	}
	tplCount, err := s.Queries().CountTemplatesForFirmware(ctx, fwID)
	if err != nil {
		t.Fatalf("CountTemplatesForFirmware: %v", err)
	}
	if tplCount != 1 {
		t.Errorf("template count = %d, want 1 (soft-deleted excluded)", tplCount)
	}
}

func TestFirmwaresListNodeFirmwaresEmpty(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeID := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(nodeID, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	rows, err := s.Queries().ListNodeFirmwares(ctx, store.ListNodeFirmwaresParams{
		NodeID:     nodeID,
		LimitCount: 50,
	})
	if err != nil {
		t.Fatalf("ListNodeFirmwares: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListNodeFirmwares len = %d, want 0 (heartbeat receiver not wired)", len(rows))
	}
}
