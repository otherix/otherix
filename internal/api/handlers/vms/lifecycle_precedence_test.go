// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// precedenceStoreSpy satisfies the handler's Store interface for the T16b
// lifecycle-precedence tests. It embeds Store (nil) so only the methods the
// stop/delete enqueue path touches have bodies; any other call panics. It
// records the CancelMigration and EnqueueTask invocations so the test can
// assert the ordering contract (cancel-then-enqueue).
type precedenceStoreSpy struct {
	Store

	vm        store.VM
	active    store.Migration // returned by ActiveMigrationForVM when hasActive
	hasActive bool

	cancelledID     uuid.UUID
	cancelledReason string
	cancelCalls     int
	enqueueCalls    int
	enqueueType     string
}

func (s *precedenceStoreSpy) VMByName(context.Context, string) (store.VM, error) {
	return s.vm, nil
}

func (s *precedenceStoreSpy) ActiveMigrationForVM(context.Context, uuid.UUID) (store.Migration, bool, error) {
	if !s.hasActive {
		return store.Migration{}, false, nil
	}
	return s.active, true, nil
}

func (s *precedenceStoreSpy) CancelMigration(_ context.Context, id uuid.UUID, reason string) (store.Migration, error) {
	s.cancelCalls++
	s.cancelledID = id
	s.cancelledReason = reason
	m := s.active
	m.Phase = store.MigrationPhaseCancelled
	return m, nil
}

func (s *precedenceStoreSpy) EnqueueTask(_ context.Context, p store.CreateTaskParams, _ queue.JobArgs) (uuid.UUID, error) {
	s.enqueueCalls++
	s.enqueueType = p.Type
	return p.ID, nil
}

func stopRequest(caller *auth.User, vmName string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/"+vmName+"/stop", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	return req.WithContext(ctx)
}

func precedenceVM(owner, pinned uuid.UUID) store.VM {
	pin := pinned
	return store.VM{
		ID: uuid.New(), OwnerID: owner, Name: "prec-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingScheduled,
		CpuCores:         2, MemoryMib: 2048, PinnedNodeID: &pin,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

// TestStop_CancelsActiveMigration locks T16b for the stop path: a VM with an
// active migration, on stop, cancels the migration first (with the
// "superseded by vm stop" audit reason) AND still enqueues the vm.stop task.
func TestStop_CancelsActiveMigration(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	pinned := uuid.New()
	vm := precedenceVM(owner, pinned)
	target := uuid.New()
	mig := store.Migration{
		ID: uuid.New(), VmID: vm.ID, TargetNodeID: &target,
		Phase: store.MigrationPhaseActive, ProgressPercent: 30,
	}
	spy := &precedenceStoreSpy{vm: vm, active: mig, hasActive: true}
	h := discardHandler(spy)

	rec := httptest.NewRecorder()
	h.Stop(rec, stopRequest(&auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}, vm.Name))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.cancelCalls != 1 {
		t.Errorf("CancelMigration calls = %d, want 1", spy.cancelCalls)
	}
	if spy.cancelledID != mig.ID {
		t.Errorf("cancelled migration id = %v, want %v", spy.cancelledID, mig.ID)
	}
	if spy.cancelledReason != "superseded by vm stop" {
		t.Errorf("cancel reason = %q, want %q", spy.cancelledReason, "superseded by vm stop")
	}
	if spy.enqueueCalls != 1 || spy.enqueueType != "vm.stop" {
		t.Errorf("enqueue = (%d, %q), want (1, vm.stop)", spy.enqueueCalls, spy.enqueueType)
	}
}

// TestStop_NoMigration_NoCancel locks the scope-care half: with no active
// migration, stop enqueues without calling CancelMigration.
func TestStop_NoMigration_NoCancel(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	vm := precedenceVM(owner, uuid.New())
	spy := &precedenceStoreSpy{vm: vm, hasActive: false}
	h := discardHandler(spy)

	rec := httptest.NewRecorder()
	h.Stop(rec, stopRequest(&auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}, vm.Name))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	if spy.cancelCalls != 0 {
		t.Errorf("CancelMigration calls = %d, want 0 (no active migration)", spy.cancelCalls)
	}
	if spy.enqueueCalls != 1 || spy.enqueueType != "vm.stop" {
		t.Errorf("enqueue = (%d, %q), want (1, vm.stop)", spy.enqueueCalls, spy.enqueueType)
	}
}
