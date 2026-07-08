// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// idemScanSpy satisfies the storagepools Handler's Store interface for the
// exactly-once threading tests. EnqueueTask mirrors etcdstore's guarded
// contract (see the vms package spy for the full note): descriptor present ->
// exactly-once by (user,key), same-key/different-hash -> ErrIdempotencyKeyMismatch.
type idemScanSpy struct {
	Store

	pool store.StoragePool
	node store.Node

	created int
	index   map[string]idemScanEntry
}

type idemScanEntry struct {
	taskID uuid.UUID
	hash   []byte
}

func (s *idemScanSpy) StoragePoolByID(context.Context, uuid.UUID) (store.StoragePool, error) {
	return s.pool, nil
}

func (s *idemScanSpy) NodeByID(context.Context, uuid.UUID) (store.Node, error) {
	return s.node, nil
}

func (s *idemScanSpy) EnqueueTask(_ context.Context, p store.CreateTaskParams, _ queue.JobArgs) (uuid.UUID, error) {
	if p.IdempotencyUserID == nil || p.IdempotencyKey == nil {
		s.created++
		return p.ID, nil
	}
	if s.index == nil {
		s.index = map[string]idemScanEntry{}
	}
	k := p.IdempotencyUserID.String() + "\x00" + *p.IdempotencyKey
	if prior, ok := s.index[k]; ok {
		if string(prior.hash) != string(p.IdempotencyHash) {
			return uuid.Nil, store.ErrIdempotencyKeyMismatch
		}
		return prior.taskID, nil
	}
	s.created++
	s.index[k] = idemScanEntry{taskID: p.ID, hash: p.IdempotencyHash}
	return p.ID, nil
}

func idemScanHandler(s Store) *Handler {
	return New(s, config.StoragePoolsConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func idemScanSpyFixture() *idemScanSpy {
	poolID := uuid.New()
	nodeID := uuid.New()
	return &idemScanSpy{
		pool: store.StoragePool{ID: poolID, NodeID: nodeID, Name: "pool-a", Type: "local_dir"},
		node: store.Node{ID: nodeID, Status: store.NodeStatusReady},
	}
}

func idemScanRequest(poolID uuid.UUID, caller *auth.User, d *middleware.IdempotencyDescriptor) *http.Request {
	id := poolID.String()
	req := httptest.NewRequest(http.MethodPost, "/v1/storage-pools/"+id+"/scan", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithUser(ctx, caller)
	if d != nil {
		ctx = middleware.WithIdempotency(ctx, d)
	}
	return req.WithContext(ctx)
}

func scanTaskID(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode accepted body: %v (body=%s)", err, body)
	}
	return got.TaskID
}

func scanErrCode(t *testing.T, body []byte) string {
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

// TestScan_IdempotentReplay_SameTaskID locks the scan leg: a descriptor present
// makes the guarded EnqueueTask exactly-once - a replay with the same key+hash
// returns the SAME task id and creates exactly one task.
func TestScan_IdempotentReplay_SameTaskID(t *testing.T) {
	t.Parallel()

	spy := idemScanSpyFixture()
	h := idemScanHandler(spy)
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleOperator, Type: auth.TypeJWT}
	d := &middleware.IdempotencyDescriptor{Key: "k-scan", Hash: []byte("hashA")}

	rec1 := httptest.NewRecorder()
	h.Scan(rec1, idemScanRequest(spy.pool.ID, caller, d))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.Scan(rec2, idemScanRequest(spy.pool.ID, caller, d))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202 (body=%s)", rec2.Code, rec2.Body.String())
	}

	if got := spy.created; got != 1 {
		t.Errorf("tasks created = %d, want 1 (exactly-once)", got)
	}
	if id1, id2 := scanTaskID(t, rec1.Body.Bytes()), scanTaskID(t, rec2.Body.Bytes()); id1 != id2 {
		t.Errorf("replay task_id = %q, want %q (same task)", id2, id1)
	}
}

// TestScan_IdempotencyMismatch_409 locks the fail-closed leg: same key, a
// different body hash surfaces as 409 idempotency_key_mismatch.
func TestScan_IdempotencyMismatch_409(t *testing.T) {
	t.Parallel()

	spy := idemScanSpyFixture()
	h := idemScanHandler(spy)
	caller := &auth.User{ID: uuid.New(), Role: auth.RoleOperator, Type: auth.TypeJWT}

	rec1 := httptest.NewRecorder()
	h.Scan(rec1, idemScanRequest(spy.pool.ID, caller,
		&middleware.IdempotencyDescriptor{Key: "k-scan", Hash: []byte("hashA")}))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202 (body=%s)", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.Scan(rec2, idemScanRequest(spy.pool.ID, caller,
		&middleware.IdempotencyDescriptor{Key: "k-scan", Hash: []byte("hashB")}))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409 (body=%s)", rec2.Code, rec2.Body.String())
	}
	if got := scanErrCode(t, rec2.Body.Bytes()); got != "idempotency_key_mismatch" {
		t.Errorf("error code = %q, want idempotency_key_mismatch", got)
	}
}
