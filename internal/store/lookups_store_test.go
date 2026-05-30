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

// The identifier-lookup methods on *Store back the API resolver. Each
// single-row lookup translates pgx.ErrNoRows into store.ErrNotFound so
// the resolver (and its callers) depend on the store sentinel, not pgx.

func TestNodeByNameNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.NodeByName(context.Background(), "absent-"+uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NodeByName(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestVMByNameNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.VMByName(context.Background(), "absent-"+uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VMByName(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestTemplateByNameNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.TemplateByName(context.Background(), "absent-"+uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TemplateByName(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestStoragePoolByIDNotFound(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	if _, err := s.StoragePoolByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("StoragePoolByID(absent) error = %v, want store.ErrNotFound", err)
	}
}

func TestStoragePoolsByNameEmptyNoError(t *testing.T) {
	requireSharedHarness(t)
	s := newStore(t, sharedHarness)
	rows, err := s.StoragePoolsByName(context.Background(), "absent-"+uuid.NewString())
	if err != nil {
		t.Errorf("StoragePoolsByName(absent) error = %v, want nil (empty slice)", err)
	}
	if len(rows) != 0 {
		t.Errorf("StoragePoolsByName(absent) len = %d, want 0", len(rows))
	}
}
