// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// idemStoreSpy satisfies the vms Handler's Store interface for the
// exactly-once threading tests. It mirrors etcdstore.EnqueueTask's guarded
// contract: with a full idempotency descriptor the first commit under a
// (user,key) wins and later calls with the same hash replay its task id, while
// a same-key/different-hash call fails closed with ErrIdempotencyKeyMismatch.
// With no descriptor it always creates. It records the number of real creates
// so a test can prove exactly-once.
type idemStoreSpy struct {
	Store

	vm store.VM

	created int
	index   map[string]idemEntry
}

type idemEntry struct {
	taskID uuid.UUID
	hash   []byte
}

func (s *idemStoreSpy) VMByName(context.Context, string) (store.VM, error) {
	return s.vm, nil
}

func (s *idemStoreSpy) ActiveMigrationForVM(context.Context, uuid.UUID) (store.Migration, bool, error) {
	return store.Migration{}, false, nil
}

func (s *idemStoreSpy) EnqueueTask(_ context.Context, p store.CreateTaskParams, _ queue.JobArgs) (uuid.UUID, error) {
	if p.IdempotencyUserID == nil || p.IdempotencyKey == nil {
		s.created++
		return p.ID, nil
	}
	if s.index == nil {
		s.index = map[string]idemEntry{}
	}
	k := p.IdempotencyUserID.String() + "\x00" + *p.IdempotencyKey
	if prior, ok := s.index[k]; ok {
		if string(prior.hash) != string(p.IdempotencyHash) {
			return uuid.Nil, store.ErrIdempotencyKeyMismatch
		}
		return prior.taskID, nil
	}
	s.created++
	s.index[k] = idemEntry{taskID: p.ID, hash: p.IdempotencyHash}
	return p.ID, nil
}

func idemVM(owner uuid.UUID) store.VM {
	pin := uuid.New()
	return store.VM{
		ID: uuid.New(), OwnerID: owner, Name: "idem-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingScheduled,
		CpuCores:         2, MemoryMib: 2048, PinnedNodeID: &pin,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

// idemRequest builds a POST/DELETE request carrying the authenticated caller
// and, when d != nil, the idempotency descriptor the handler threads into
// EnqueueTask.
func idemRequest(method, path, vmName string, caller *auth.User, d *middleware.IdempotencyDescriptor) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", vmName)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	if d != nil {
		ctx = middleware.WithIdempotency(ctx, d)
	}
	return req.WithContext(ctx)
}

func decodeTaskID(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode accepted body: %v (body=%s)", err, body)
	}
	return got.TaskID
}

func decodeErrCode(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, body)
	}
	return got.Error.Code
}

// TestDelete_IdempotentReplay_SameTaskID locks the delete leg: a descriptor
// present on the context makes the guarded EnqueueTask exactly-once - a replay
// with the same key+hash returns the SAME task id and creates exactly one task.
func TestDelete_IdempotentReplay_SameTaskID(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	spy := &idemStoreSpy{vm: idemVM(owner)}
	h := discardHandler(spy)
	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}
	d := &middleware.IdempotencyDescriptor{Key: "k-del", Hash: []byte("hashA")}

	rec1 := httptest.NewRecorder()
	h.Delete(rec1, idemRequest(http.MethodDelete, "/v1/vms/"+spy.vm.Name, spy.vm.Name, caller, d))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.Delete(rec2, idemRequest(http.MethodDelete, "/v1/vms/"+spy.vm.Name, spy.vm.Name, caller, d))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202 (body=%s)", rec2.Code, rec2.Body.String())
	}

	if got := spy.created; got != 1 {
		t.Errorf("tasks created = %d, want 1 (exactly-once)", got)
	}
	if id1, id2 := decodeTaskID(t, rec1.Body.Bytes()), decodeTaskID(t, rec2.Body.Bytes()); id1 != id2 {
		t.Errorf("replay task_id = %q, want %q (same task)", id2, id1)
	}
}

// TestDelete_IdempotencyMismatch_409 locks the fail-closed leg: same key, a
// different body hash surfaces as 409 idempotency_key_mismatch.
func TestDelete_IdempotencyMismatch_409(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	spy := &idemStoreSpy{vm: idemVM(owner)}
	h := discardHandler(spy)
	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}

	rec1 := httptest.NewRecorder()
	h.Delete(rec1, idemRequest(http.MethodDelete, "/v1/vms/"+spy.vm.Name, spy.vm.Name, caller,
		&middleware.IdempotencyDescriptor{Key: "k-del", Hash: []byte("hashA")}))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.Delete(rec2, idemRequest(http.MethodDelete, "/v1/vms/"+spy.vm.Name, spy.vm.Name, caller,
		&middleware.IdempotencyDescriptor{Key: "k-del", Hash: []byte("hashB")}))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409 (body=%s)", rec2.Code, rec2.Body.String())
	}
	if got := decodeErrCode(t, rec2.Body.Bytes()); got != "idempotency_key_mismatch" {
		t.Errorf("error code = %q, want idempotency_key_mismatch", got)
	}
}

// TestStart_IdempotentReplay_SameTaskID locks the async-lifecycle leg (Start
// stands in for the four ops that share runAsyncLifecycleEnqueue).
func TestStart_IdempotentReplay_SameTaskID(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	spy := &idemStoreSpy{vm: idemVM(owner)}
	h := discardHandler(spy)
	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}
	d := &middleware.IdempotencyDescriptor{Key: "k-start", Hash: []byte("hashA")}

	rec1 := httptest.NewRecorder()
	h.Start(rec1, idemRequest(http.MethodPost, "/v1/vms/"+spy.vm.Name+"/start", spy.vm.Name, caller, d))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.Start(rec2, idemRequest(http.MethodPost, "/v1/vms/"+spy.vm.Name+"/start", spy.vm.Name, caller, d))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202 (body=%s)", rec2.Code, rec2.Body.String())
	}

	if got := spy.created; got != 1 {
		t.Errorf("tasks created = %d, want 1 (exactly-once)", got)
	}
	if id1, id2 := decodeTaskID(t, rec1.Body.Bytes()), decodeTaskID(t, rec2.Body.Bytes()); id1 != id2 {
		t.Errorf("replay task_id = %q, want %q (same task)", id2, id1)
	}
}

// TestStart_IdempotencyMismatch_409 locks the fail-closed leg for lifecycle.
func TestStart_IdempotencyMismatch_409(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	spy := &idemStoreSpy{vm: idemVM(owner)}
	h := discardHandler(spy)
	caller := &auth.User{ID: owner, Role: auth.RoleOperator, Type: auth.TypeJWT}

	rec1 := httptest.NewRecorder()
	h.Start(rec1, idemRequest(http.MethodPost, "/v1/vms/"+spy.vm.Name+"/start", spy.vm.Name, caller,
		&middleware.IdempotencyDescriptor{Key: "k-start", Hash: []byte("hashA")}))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.Start(rec2, idemRequest(http.MethodPost, "/v1/vms/"+spy.vm.Name+"/start", spy.vm.Name, caller,
		&middleware.IdempotencyDescriptor{Key: "k-start", Hash: []byte("hashB")}))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409 (body=%s)", rec2.Code, rec2.Body.String())
	}
	if got := decodeErrCode(t, rec2.Body.Bytes()); got != "idempotency_key_mismatch" {
		t.Errorf("error code = %q, want idempotency_key_mismatch", got)
	}
}
