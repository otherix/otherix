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

	"github.com/otherix/otherix/internal/store"
)

// The firmware domain methods on *Store wrap the generated queries and
// translate driver errors (pgx.ErrNoRows, pgconn unique violations)
// into store-level sentinels so handlers depend on the store package,
// not on pgx. These tests pin that translation contract.

func TestFirmwareByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	_, err := s.FirmwareByID(ctx, uuid.New())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FirmwareByID(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestDefaultFirmwareForArchTypeNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	_, err := s.DefaultFirmwareForArchType(ctx, store.CpuArchArm64, store.FirmwareTypeBios)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DefaultFirmwareForArchType(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestCreateFirmwareDuplicateNameArch(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	name := uniqueFirmwareName("dup")
	if _, err := s.CreateFirmware(ctx, defaultFirmwareParams(uuid.New(), name)); err != nil {
		t.Fatalf("first CreateFirmware: %v", err)
	}
	_, err := s.CreateFirmware(ctx, defaultFirmwareParams(uuid.New(), name))
	if !errors.Is(err, store.ErrFirmwareNameExists) {
		t.Errorf("duplicate CreateFirmware error = %v, want store.ErrFirmwareNameExists", err)
	}
}

func TestCreateFirmwareSecondDefaultRejected(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	first := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("def-a"))
	first.IsDefault = true
	if _, err := s.CreateFirmware(ctx, first); err != nil {
		t.Fatalf("first default CreateFirmware: %v", err)
	}
	// The (amd64, uefi) default is a global singleton (uq_firmwares_default
	// is a partial unique index); the shared harness has no per-test row
	// cleanup, so leaving it behind would collide with other default tests.
	t.Cleanup(func() {
		if err := s.DeleteFirmware(context.Background(), first.ID); err != nil {
			t.Errorf("cleanup DeleteFirmware: %v", err)
		}
	})

	second := defaultFirmwareParams(uuid.New(), uniqueFirmwareName("def-b"))
	second.IsDefault = true
	_, err := s.CreateFirmware(ctx, second)
	if !errors.Is(err, store.ErrFirmwareDefaultExists) {
		t.Errorf("second default CreateFirmware error = %v, want store.ErrFirmwareDefaultExists", err)
	}
}

func TestDeleteFirmwareNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	err := s.DeleteFirmware(ctx, uuid.New())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteFirmware(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteFirmwareInUse(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	fwID := uuid.New()
	if _, err := s.CreateFirmware(ctx, defaultFirmwareParams(fwID, uniqueFirmwareName("inuse"))); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}

	ownerID := uuid.New()
	if _, err := s.Pool().Exec(ctx,
		`insert into users (id, email, password_hash, role) values ($1, $2, 'x', 'developer')`,
		ownerID, "fwinuse-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`insert into vms (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type, firmware_id)
		 values ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0', $4)`,
		uuid.New(), ownerID, "vm-"+uuid.NewString()[:8], fwID); err != nil {
		t.Fatalf("insert vm: %v", err)
	}

	err := s.DeleteFirmware(ctx, fwID)
	var inUse *store.ResourceInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("DeleteFirmware(in use) error = %v, want *store.ResourceInUseError", err)
	}
	if inUse.Resources["vms"] != 1 {
		t.Errorf("Resources[vms] = %d, want 1", inUse.Resources["vms"])
	}
	if _, ok := inUse.Resources["templates"]; ok {
		t.Errorf("Resources has templates key = %v, want absent (no templates reference)", inUse.Resources["templates"])
	}
}

func TestDeleteFirmwareSucceeds(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.CreateFirmware(ctx, defaultFirmwareParams(id, uniqueFirmwareName("del"))); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	if err := s.DeleteFirmware(ctx, id); err != nil {
		t.Fatalf("DeleteFirmware: %v", err)
	}
	if _, err := s.FirmwareByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after delete FirmwareByID error = %v, want store.ErrNotFound", err)
	}
}
