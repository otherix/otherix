// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

// activeDeleteStoreStub satisfies the handler's Store interface for the
// vmDeleting fail-soft test. It embeds Store (nil) so only
// ActiveVMDeleteTaskVMIDs has a body; any other call panics.
type activeDeleteStoreStub struct {
	Store
	set map[uuid.UUID]struct{}
	err error
}

func (s *activeDeleteStoreStub) ActiveVMDeleteTaskVMIDs(context.Context) (map[uuid.UUID]struct{}, error) {
	return s.set, s.err
}

func discardHandler(store Store) *Handler {
	return &Handler{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestVMDeleting_FailSoft locks the reliability fix: a task-scan error must
// degrade to deleting=false (the VM still renders, no 500), not propagate.
func TestVMDeleting_FailSoft(t *testing.T) {
	t.Parallel()
	h := discardHandler(&activeDeleteStoreStub{err: errors.New("etcd unavailable")})
	if got := h.vmDeleting(context.Background(), uuid.New()); got {
		t.Errorf("vmDeleting on scan error = true, want false (fail-soft)")
	}
}

// TestVMDeleting_Membership covers the happy path: a VM with an active
// vm.delete task reports true, one without reports false.
func TestVMDeleting_Membership(t *testing.T) {
	t.Parallel()
	deleting := uuid.New()
	other := uuid.New()
	h := discardHandler(&activeDeleteStoreStub{set: map[uuid.UUID]struct{}{deleting: {}}})
	if !h.vmDeleting(context.Background(), deleting) {
		t.Errorf("vmDeleting(active) = false, want true")
	}
	if h.vmDeleting(context.Background(), other) {
		t.Errorf("vmDeleting(inactive) = true, want false")
	}
}
