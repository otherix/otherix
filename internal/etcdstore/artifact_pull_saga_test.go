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

	"github.com/otherix/otherix/internal/etcdstore"
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

func TestDeleteExpiredPullSagas(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()

	oldTerminal := seedPullSaga(t, st, ctx, store.PullSagaPhaseComplete, time.Now().Add(-2*time.Hour))
	freshTerminal := seedPullSaga(t, st, ctx, store.PullSagaPhaseFailed, time.Now())
	oldPending := seedPullSaga(t, st, ctx, store.PullSagaPhasePulling, time.Now().Add(-2*time.Hour))

	cutoff := time.Now().Add(-time.Hour)
	deleted, err := st.DeleteExpiredPullSagas(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteExpiredPullSagas = %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteExpiredPullSagas deleted = %d, want 1 (only terminal+old)", deleted)
	}
	if _, err := st.PullSagaByID(ctx, oldTerminal); err == nil {
		t.Errorf("terminal+old saga still present, want deleted")
	}
	if _, err := st.PullSagaByID(ctx, freshTerminal); err != nil {
		t.Errorf("terminal+fresh saga deleted, want kept: %v", err)
	}
	if _, err := st.PullSagaByID(ctx, oldPending); err != nil {
		t.Errorf("non-terminal+old saga deleted, want kept (stuck saga is not retention's job): %v", err)
	}
}

// seedPullSaga creates a saga at the given phase and CreatedAt by constructing
// the value and writing it through the store's test-only put helper (the public
// create path stamps CreatedAt itself, so a chosen timestamp needs the seam).
func seedPullSaga(t *testing.T, st *etcdstore.Store, ctx context.Context, phase store.PullSagaPhase, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	saga := store.ArtifactPullSaga{
		ID:           id,
		Digest:       "abababababababababababababababababababababababababababababababab",
		ConsumerNode: uuid.New(),
		HolderNode:   uuid.New(),
		Phase:        phase,
		CreatedAt:    createdAt,
	}
	if err := st.PutPullSagaForTest(ctx, saga); err != nil {
		t.Fatalf("PutPullSagaForTest = %v", err)
	}
	return id
}
