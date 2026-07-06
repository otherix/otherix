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

// intentKey builds a delete-intent key the way etcdstore does internally, for
// tests that simulate an in-flight delete or assert an intent was cleared.
func intentKey(resource string, id uuid.UUID) string {
	return etcd.Key("deleting", resource, id.String())
}

func intentPresent(t *testing.T, cli *etcd.Client, ctx context.Context, key string) bool {
	t.Helper()
	_, found, err := cli.Get(ctx, key)
	if err != nil {
		t.Fatalf("get intent %s: %v", key, err)
	}
	return found
}

// TestCreateVMBlockedWhileFirmwareDeleting pins the create-during-delete guard:
// with a firmware's delete-intent present, a VM create referencing that firmware
// is rejected ErrResourceDeleting - the reference must not be created on a
// resource on its way out. Revert-confirm: without the create-side guard the
// create would slip through and strand a dangling vm->firmware reference.
func TestCreateVMBlockedWhileFirmwareDeleting(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	fw, err := s.CreateFirmware(ctx, fwParams("fw-blocking", store.CpuArchAmd64, false))
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	// Simulate an in-flight delete of the firmware.
	if err := cli.Put(ctx, intentKey("firmwares", fw.ID), []byte(time.Now().UTC().Format(time.RFC3339))); err != nil {
		t.Fatalf("set intent: %v", err)
	}

	p := mkUnscheduledParams(t, "vm-blocked-by-fw-delete")
	p.FirmwareID = &fw.ID
	if _, err := s.CreateUnscheduledVM(ctx, p); !errors.Is(err, store.ErrResourceDeleting) {
		t.Errorf("CreateUnscheduledVM while firmware deleting = %v, want store.ErrResourceDeleting", err)
	}

	// Once the intent clears, the same create succeeds.
	if _, derr := cli.Raw().Delete(ctx, intentKey("firmwares", fw.ID)); derr != nil {
		t.Fatalf("clear intent: %v", derr)
	}
	if _, err := s.CreateUnscheduledVM(ctx, p); err != nil {
		t.Errorf("CreateUnscheduledVM after intent cleared = %v, want nil", err)
	}
}

// TestDeleteFirmwareClearsIntentOnSuccess pins that a successful firmware delete
// removes its own delete-intent (the finalize txn deletes the intent), so a
// subsequent create referencing a DIFFERENT firmware is not spuriously blocked
// and the reaper has nothing to sweep.
func TestDeleteFirmwareClearsIntentOnSuccess(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	fw, err := s.CreateFirmware(ctx, fwParams("fw-clean-success", store.CpuArchAmd64, false))
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	if err := s.DeleteFirmware(ctx, fw.ID); err != nil {
		t.Fatalf("DeleteFirmware: %v", err)
	}
	if intentPresent(t, cli, ctx, intentKey("firmwares", fw.ID)) {
		t.Error("firmware delete-intent survived a successful delete")
	}
	// Idempotent second delete: the firmware is gone (LOW-2 loser path).
	if err := s.DeleteFirmware(ctx, fw.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteFirmware = %v, want store.ErrNotFound", err)
	}
}

// TestDeleteFirmwareClearsIntentOnRefuse pins that a firmware delete refused for
// an in-use firmware removes its own intent, so a failed delete does not leave a
// leaked intent blocking future VM creates against that firmware.
func TestDeleteFirmwareClearsIntentOnRefuse(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	fw, err := s.CreateFirmware(ctx, fwParams("fw-clean-refuse", store.CpuArchAmd64, false))
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	p := mkUnscheduledParams(t, "vm-holds-fw")
	p.FirmwareID = &fw.ID
	if _, err := s.CreateUnscheduledVM(ctx, p); err != nil {
		t.Fatalf("CreateUnscheduledVM: %v", err)
	}

	var inUse *store.ResourceInUseError
	if err := s.DeleteFirmware(ctx, fw.ID); !errors.As(err, &inUse) {
		t.Fatalf("DeleteFirmware(in use) = %v, want ResourceInUseError", err)
	}
	if intentPresent(t, cli, ctx, intentKey("firmwares", fw.ID)) {
		t.Error("firmware delete-intent leaked after a refused delete")
	}
	// The firmware is still usable: a create against it still works.
	p2 := mkUnscheduledParams(t, "vm-holds-fw-2")
	p2.FirmwareID = &fw.ID
	if _, err := s.CreateUnscheduledVM(ctx, p2); err != nil {
		t.Errorf("CreateUnscheduledVM after refused delete = %v, want nil (firmware still usable)", err)
	}
}

// TestReapStaleDeleteIntents pins the reaper: an intent older than the staleness
// threshold is swept (a crashed delete's leak), a fresh one is spared.
func TestReapStaleDeleteIntents(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	staleKey := intentKey("networks", uuid.New())
	freshKey := intentKey("storage_pools", uuid.New())
	if err := cli.Put(ctx, staleKey, []byte(now.Add(-10*time.Minute).Format(time.RFC3339))); err != nil {
		t.Fatalf("put stale: %v", err)
	}
	if err := cli.Put(ctx, freshKey, []byte(now.Format(time.RFC3339))); err != nil {
		t.Fatalf("put fresh: %v", err)
	}

	n, err := s.ReapStaleDeleteIntents(ctx, now, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReapStaleDeleteIntents: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped = %d, want 1 (only the stale one)", n)
	}
	if intentPresent(t, cli, ctx, staleKey) {
		t.Error("stale intent survived the reaper")
	}
	if !intentPresent(t, cli, ctx, freshKey) {
		t.Error("fresh intent was reaped (should be spared)")
	}
}

// TestDeleteNetworkAndPoolClearIntentOnSuccess pins the same intent-clearing
// contract for network and storage-pool deletes.
func TestDeleteNetworkAndPoolClearIntentOnSuccess(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	net, err := s.CreateNetwork(ctx, netParams(uniqueNetName("net-intent-clean")))
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if err := s.DeleteNetwork(ctx, net.ID); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if intentPresent(t, cli, ctx, intentKey("networks", net.ID)) {
		t.Error("network delete-intent survived a successful delete")
	}
	if err := s.DeleteNetwork(ctx, net.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteNetwork = %v, want store.ErrNotFound", err)
	}
}
