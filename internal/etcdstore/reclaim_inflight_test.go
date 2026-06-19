//go:build integration

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTryBeginReclaim(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	digest := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	node := uuid.New()

	began, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim first = %v, want nil", err)
	}
	if !began {
		t.Errorf("TryBeginReclaim first = false, want true (fresh marker)")
	}

	began2, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim second = %v, want nil", err)
	}
	if began2 {
		t.Errorf("TryBeginReclaim second = true, want false (marker still fresh)")
	}

	if err := st.EndReclaim(ctx, digest, node); err != nil {
		t.Fatalf("EndReclaim = %v, want nil", err)
	}

	began3, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim after end = %v, want nil", err)
	}
	if !began3 {
		t.Errorf("TryBeginReclaim after EndReclaim = false, want true (marker cleared)")
	}
}

func TestTryBeginReclaimExpired(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	digest := "00f100f100f100f100f100f100f100f100f100f100f100f100f100f100f100f1"
	node := uuid.New()

	began, err := st.TryBeginReclaim(ctx, digest, node, -time.Second)
	if err != nil {
		t.Fatalf("TryBeginReclaim = %v, want nil", err)
	}
	if !began {
		t.Errorf("TryBeginReclaim = false, want true")
	}
	began2, err := st.TryBeginReclaim(ctx, digest, node, time.Minute)
	if err != nil {
		t.Fatalf("TryBeginReclaim over expired = %v, want nil", err)
	}
	if !began2 {
		t.Errorf("TryBeginReclaim over expired marker = false, want true")
	}
}
