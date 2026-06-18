// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestPullSagaLifecycle(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	consumer, holder := uuid.New(), uuid.New()
	digest := "00000000000000000000000000000000000000000000000000000000000000ef"

	saga, plaintext, err := s.CreatePullSaga(ctx, store.CreatePullSagaParams{
		ID:           uuid.New(),
		Digest:       digest,
		ConsumerNode: consumer,
		HolderNode:   holder,
		TokenTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePullSaga: %v", err)
	}
	if plaintext == "" {
		t.Fatalf("CreatePullSaga returned empty token plaintext")
	}

	got, err := s.PullSagaByID(ctx, saga.ID)
	if err != nil || got.Digest != digest || got.ConsumerNode != consumer || got.HolderNode != holder {
		t.Fatalf("PullSagaByID = %+v, %v", got, err)
	}
	if got.Phase != store.PullSagaPhasePending {
		t.Errorf("PullSagaByID phase = %q, want %q", got.Phase, store.PullSagaPhasePending)
	}

	if err := s.UpdatePullSagaServeEndpoint(ctx, saga.ID, "https://holder:49200"); err != nil {
		t.Fatalf("UpdatePullSagaServeEndpoint: %v", err)
	}
	got, err = s.PullSagaByID(ctx, saga.ID)
	if err != nil || got.ServeEndpoint != "https://holder:49200" || got.Phase != store.PullSagaPhaseServing {
		t.Fatalf("after UpdatePullSagaServeEndpoint = %+v, %v", got, err)
	}

	if err := s.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseComplete); err != nil {
		t.Fatalf("SetPullSagaPhase: %v", err)
	}
	got, err = s.PullSagaByID(ctx, saga.ID)
	if err != nil || got.Phase != store.PullSagaPhaseComplete {
		t.Fatalf("after SetPullSagaPhase = %+v, %v", got, err)
	}
}
