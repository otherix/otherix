// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

func TestSessionCA_CreateIdempotentAndActive(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if _, err := s.ActiveSessionCA(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ActiveSessionCA on empty store error = %v, want ErrNotFound", err)
	}
	mat, err := auth.GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA() error = %v", err)
	}
	row, err := s.CreateSessionCA(ctx, store.CreateSessionCAParams{
		PrivateKeyPEM: mat.PrivateKeyPEM, PublicKeyPEM: mat.PublicKeyPEM,
	})
	if err != nil {
		t.Fatalf("CreateSessionCA() error = %v", err)
	}
	// Second create loses the race and returns the SAME row.
	mat2, _ := auth.GenerateSessionCA()
	row2, err := s.CreateSessionCA(ctx, store.CreateSessionCAParams{
		PrivateKeyPEM: mat2.PrivateKeyPEM, PublicKeyPEM: mat2.PublicKeyPEM,
	})
	if err != nil {
		t.Fatalf("CreateSessionCA() second error = %v", err)
	}
	if row2.ID != row.ID {
		t.Errorf("second CreateSessionCA id = %v, want winner %v", row2.ID, row.ID)
	}
	active, err := s.ActiveSessionCA(ctx)
	if err != nil {
		t.Fatalf("ActiveSessionCA() error = %v", err)
	}
	if active.ID != row.ID {
		t.Errorf("active id = %v, want %v", active.ID, row.ID)
	}
}

func TestSessionCA_ConcurrentCreateConverges(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	const racers = 8
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = make([]uuid.UUID, 0, racers)
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			mat, err := auth.GenerateSessionCA()
			if err != nil {
				t.Errorf("GenerateSessionCA() error = %v", err)
				return
			}
			row, err := s.CreateSessionCA(ctx, store.CreateSessionCAParams{
				PrivateKeyPEM: mat.PrivateKeyPEM, PublicKeyPEM: mat.PublicKeyPEM,
			})
			if err != nil {
				t.Errorf("CreateSessionCA() error = %v", err)
				return
			}
			mu.Lock()
			ids = append(ids, row.ID)
			mu.Unlock()
		}()
	}
	wg.Wait()

	active, err := s.ActiveSessionCA(ctx)
	if err != nil {
		t.Fatalf("ActiveSessionCA() error = %v", err)
	}
	for _, id := range ids {
		if id != active.ID {
			t.Errorf("racer returned id %v, want the single winner %v", id, active.ID)
		}
	}
}
