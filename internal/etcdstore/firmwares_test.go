// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

func fwParams(name string, arch store.CPUArch, isDefault bool) store.CreateFirmwareParams {
	return store.CreateFirmwareParams{
		ID:           uuid.New(),
		Name:         name,
		Architecture: arch,
		Type:         store.FirmwareTypeUefi,
		SecureBoot:   false,
		IsDefault:    isDefault,
	}
}

func uniqueFwName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestFirmwareCreateGetAndDefault(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := fwParams(uniqueFwName("fw"), store.CpuArchAmd64, true)
	created, err := s.CreateFirmware(ctx, p)
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	if created.ID != p.ID || created.CreatedAt.IsZero() {
		t.Errorf("CreateFirmware = %+v, want id + created_at", created)
	}
	got, err := s.FirmwareByID(ctx, p.ID)
	if err != nil || got.Name != p.Name {
		t.Fatalf("FirmwareByID = (%+v, %v)", got, err)
	}
	def, err := s.DefaultFirmwareForArchType(ctx, store.CpuArchAmd64, store.FirmwareTypeUefi)
	if err != nil || def.ID != p.ID {
		t.Errorf("DefaultFirmwareForArchType = (%+v, %v), want id %v", def, err, p.ID)
	}
}

func TestFirmwareByIDNotFound(t *testing.T) {
	s, _ := startStore(t)
	if _, err := s.FirmwareByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FirmwareByID(absent) = %v, want store.ErrNotFound", err)
	}
	if _, err := s.DefaultFirmwareForArchType(context.Background(), store.CpuArchArm64, store.FirmwareTypeBios); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DefaultFirmwareForArchType(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestFirmwareNameArchUniqueness(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	name := uniqueFwName("dup")
	if _, err := s.CreateFirmware(ctx, fwParams(name, store.CpuArchAmd64, false)); err != nil {
		t.Fatalf("first CreateFirmware: %v", err)
	}
	if _, err := s.CreateFirmware(ctx, fwParams(name, store.CpuArchAmd64, false)); !errors.Is(err, store.ErrFirmwareNameExists) {
		t.Errorf("dup (name,arch) = %v, want store.ErrFirmwareNameExists", err)
	}
	// Same name, different architecture is allowed.
	if _, err := s.CreateFirmware(ctx, fwParams(name, store.CpuArchArm64, false)); err != nil {
		t.Errorf("same name different arch should be allowed: %v", err)
	}
}

func TestFirmwareDefaultUniqueness(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.CreateFirmware(ctx, fwParams(uniqueFwName("d1"), store.CpuArchAmd64, true)); err != nil {
		t.Fatalf("first default: %v", err)
	}
	if _, err := s.CreateFirmware(ctx, fwParams(uniqueFwName("d2"), store.CpuArchAmd64, true)); !errors.Is(err, store.ErrFirmwareDefaultExists) {
		t.Errorf("second default = %v, want store.ErrFirmwareDefaultExists", err)
	}
}

func TestFirmwareUpdateRenameAndDefaultToggle(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := fwParams(uniqueFwName("upd"), store.CpuArchAmd64, true)
	created, err := s.CreateFirmware(ctx, p)
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	// Toggle default off + rename.
	newName := uniqueFwName("upd-renamed")
	updated, err := s.UpdateFirmware(ctx, store.UpdateFirmwareParams{
		ID: p.ID, Name: newName, SecureBoot: true, IsDefault: false,
	})
	if err != nil {
		t.Fatalf("UpdateFirmware: %v", err)
	}
	if updated.Name != newName || !updated.SecureBoot || updated.IsDefault {
		t.Errorf("UpdateFirmware = %+v, want renamed/secureboot/non-default", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at not bumped")
	}
	// Default slot freed: a new firmware can claim it; old name reusable.
	if _, err := s.DefaultFirmwareForArchType(ctx, store.CpuArchAmd64, store.FirmwareTypeUefi); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("default slot should be free: %v", err)
	}
	if _, err := s.CreateFirmware(ctx, fwParams(p.Name, store.CpuArchAmd64, true)); err != nil {
		t.Errorf("old name + default should be reusable: %v", err)
	}
	// Now claiming default again on the updated row collides.
	if _, err := s.UpdateFirmware(ctx, store.UpdateFirmwareParams{ID: p.ID, Name: newName, IsDefault: true}); !errors.Is(err, store.ErrFirmwareDefaultExists) {
		t.Errorf("re-claim default = %v, want store.ErrFirmwareDefaultExists", err)
	}
}

func TestFirmwareListFilterAndPagination(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	var amd []uuid.UUID
	for i := 0; i < 3; i++ {
		p := fwParams(uniqueFwName("list"), store.CpuArchAmd64, false)
		if _, err := s.CreateFirmware(ctx, p); err != nil {
			t.Fatalf("seed amd %d: %v", i, err)
		}
		amd = append(amd, p.ID)
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := s.CreateFirmware(ctx, fwParams(uniqueFwName("arm"), store.CpuArchArm64, false)); err != nil {
		t.Fatalf("seed arm: %v", err)
	}
	arch := store.CpuArchAmd64
	got, err := s.ListFirmwares(ctx, store.ListFirmwaresParams{Architecture: &arch, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListFirmwares: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("ListFirmwares(amd64) len = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(got[i-1].CreatedAt) {
			t.Errorf("not ascending at %d", i)
		}
	}
	page, err := s.ListFirmwares(ctx, store.ListFirmwaresParams{
		Architecture: &arch, CursorCreatedAt: &got[0].CreatedAt, CursorID: &got[0].ID, LimitCount: 1,
	})
	if err != nil {
		t.Fatalf("ListFirmwares paged: %v", err)
	}
	if len(page) != 1 || page[0].ID != got[1].ID {
		t.Errorf("paged = %v, want [%v]", page, got[1].ID)
	}
}

func TestFirmwareDeleteAndBlocking(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	p := fwParams(uniqueFwName("del"), store.CpuArchAmd64, true)
	if _, err := s.CreateFirmware(ctx, p); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	// Block on a referencing vm.
	vmKey := etcd.Key("index", "vms", "firmware", p.ID.String(), uuid.NewString())
	if err := cli.Put(ctx, vmKey, []byte("vm")); err != nil {
		t.Fatalf("seed vm index: %v", err)
	}
	var blocking *store.ResourceInUseError
	if err := s.DeleteFirmware(ctx, p.ID); !errors.As(err, &blocking) {
		t.Fatalf("DeleteFirmware blocked = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vms"] != 1 {
		t.Errorf("blocking vms = %d, want 1", blocking.Resources["vms"])
	}
	// Remove the reference and delete succeeds; name + default reusable.
	if _, err := cli.Delete(ctx, vmKey); err != nil {
		t.Fatalf("clear vm index: %v", err)
	}
	if err := s.DeleteFirmware(ctx, p.ID); err != nil {
		t.Fatalf("DeleteFirmware: %v", err)
	}
	if _, err := s.FirmwareByID(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FirmwareByID after delete = %v, want store.ErrNotFound", err)
	}
	if _, err := s.CreateFirmware(ctx, fwParams(p.Name, store.CpuArchAmd64, true)); err != nil {
		t.Errorf("name + default not reusable after delete: %v", err)
	}
}
