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

// The tests in this file exercise the heartbeat-projection transaction
// seam (InHeartbeatTx / HeartbeatTx) backing the agent heartbeat
// receiver: the not-found translation on the lookups the projection
// branches on, and the happy-path read inside the transaction.

func TestInHeartbeatTxNodeForHeartbeatNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		if _, e := tx.NodeForHeartbeat(ctx, uuid.New()); !errors.Is(e, store.ErrNotFound) {
			t.Errorf("NodeForHeartbeat(absent) err = %v, want store.ErrNotFound", e)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InHeartbeatTx: %v", err)
	}
}

func TestInHeartbeatTxNodeByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		if _, e := tx.NodeByID(ctx, uuid.New()); !errors.Is(e, store.ErrNotFound) {
			t.Errorf("NodeByID(absent) err = %v, want store.ErrNotFound", e)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InHeartbeatTx: %v", err)
	}
}

func TestInHeartbeatTxLookupFirmwareNotFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		_, e := tx.LookupFirmwareByCatalog(ctx, store.LookupFirmwareByCatalogParams{
			Name:         "no-such-firmware-" + uuid.NewString(),
			Architecture: store.CpuArchAmd64,
			Type:         store.FirmwareTypeUefi,
		})
		if !errors.Is(e, store.ErrNotFound) {
			t.Errorf("LookupFirmwareByCatalog(absent) err = %v, want store.ErrNotFound", e)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InHeartbeatTx: %v", err)
	}
}

func TestInHeartbeatTxNodeForHeartbeatFound(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeID := seedNodeForPools(t, ctx, s)
	err := s.InHeartbeatTx(ctx, func(tx store.HeartbeatTx) error {
		row, e := tx.NodeForHeartbeat(ctx, nodeID)
		if e != nil {
			t.Errorf("NodeForHeartbeat(seeded) err = %v, want nil", e)
			return nil
		}
		if row.Architecture == "" {
			t.Errorf("NodeForHeartbeat returned empty architecture for seeded node")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InHeartbeatTx: %v", err)
	}
}

func TestInHeartbeatTxPropagatesError(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	sentinel := errors.New("projection failed")
	err := s.InHeartbeatTx(ctx, func(store.HeartbeatTx) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("InHeartbeatTx err = %v, want sentinel", err)
	}
}
