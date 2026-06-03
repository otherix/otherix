// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// fakeIdempStore is a minimal in-memory IdempotencyStore. It is not
// concurrency-safe except via its mutex; tests that exercise
// concurrency take that lock internally.
type fakeIdempStore struct {
	mu        sync.Mutex
	rows      map[string]store.IdempotencyKey
	beginErr  error
	getErr    error
	calls     fakeIdempCalls
	beginHook func()
}

type fakeIdempCalls struct {
	get      int
	begin    int
	reclaim  int
	complete int
	delete   int
}

func newFakeStore() *fakeIdempStore {
	return &fakeIdempStore{rows: map[string]store.IdempotencyKey{}}
}

func (f *fakeIdempStore) GetIdempotencyKey(_ context.Context, key string) (store.IdempotencyKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.get++
	if f.getErr != nil {
		return store.IdempotencyKey{}, f.getErr
	}
	row, ok := f.rows[key]
	if !ok {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	return row, nil
}

func (f *fakeIdempStore) BeginIdempotencyKey(_ context.Context, arg store.BeginIdempotencyKeyParams) (store.IdempotencyKey, error) {
	if f.beginHook != nil {
		f.beginHook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.begin++
	if f.beginErr != nil {
		return store.IdempotencyKey{}, f.beginErr
	}
	if _, exists := f.rows[arg.Key]; exists {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	row := store.IdempotencyKey{
		Key:           arg.Key,
		UserID:        arg.UserID,
		RequestMethod: arg.RequestMethod,
		RequestPath:   arg.RequestPath,
		RequestHash:   arg.RequestHash,
		State:         "in_flight",
		CreatedAt:     time.Now(),
		ExpiresAt:     arg.ExpiresAt,
	}
	f.rows[arg.Key] = row
	return row, nil
}

func (f *fakeIdempStore) ReclaimIdempotencyKey(_ context.Context, arg store.ReclaimIdempotencyKeyParams) (store.IdempotencyKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.reclaim++
	row, ok := f.rows[arg.Key]
	if !ok || time.Now().Before(row.ExpiresAt) {
		return store.IdempotencyKey{}, store.ErrNotFound
	}
	row.UserID = arg.UserID
	row.RequestMethod = arg.RequestMethod
	row.RequestPath = arg.RequestPath
	row.RequestHash = arg.RequestHash
	row.ResponseStatus = nil
	row.ResponseHeaders = nil
	row.ResponseBody = nil
	row.State = "in_flight"
	row.CreatedAt = time.Now()
	row.CompletedAt = nil
	row.ExpiresAt = arg.ExpiresAt
	f.rows[arg.Key] = row
	return row, nil
}

func (f *fakeIdempStore) CompleteIdempotencyKey(_ context.Context, arg store.CompleteIdempotencyKeyParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.complete++
	row, ok := f.rows[arg.Key]
	if !ok || row.State != "in_flight" {
		return nil
	}
	row.State = "completed"
	row.ResponseStatus = arg.ResponseStatus
	row.ResponseHeaders = arg.ResponseHeaders
	row.ResponseBody = arg.ResponseBody
	row.ExpiresAt = arg.ExpiresAt
	now := time.Now()
	row.CompletedAt = &now
	f.rows[arg.Key] = row
	return nil
}

func (f *fakeIdempStore) DeleteIdempotencyKey(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.delete++
	if row, ok := f.rows[key]; ok && row.State == "in_flight" {
		delete(f.rows, key)
	}
	return nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func authedRequest(method, path string, body []byte, userID uuid.UUID, key string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if key != "" {
		req.Header.Set(HeaderIdempotencyKey, key)
	}
	if userID != uuid.Nil {
		ctx := auth.WithUser(req.Context(), &auth.User{
			ID: userID, Role: auth.RoleAdmin, Type: auth.TypeJWT,
		})
		req = req.WithContext(ctx)
	}
	return req
}

func okHandler(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestIdempotency_PassthroughWithoutHeader(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())

	called := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	req := authedRequest(http.MethodPost, "/v1/users", nil, uuid.New(), "")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if called != 1 {
		t.Errorf("downstream called %d times, want 1", called)
	}
	if fake.calls.get != 0 || fake.calls.begin != 0 {
		t.Errorf("store touched without header: %+v", fake.calls)
	}
}

func TestIdempotency_PassthroughForGet(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())

	h := mw(okHandler(http.StatusOK, `{}`))
	req := authedRequest(http.MethodGet, "/v1/users", nil, uuid.New(), "any-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	if fake.calls.get != 0 {
		t.Errorf("store consulted for GET request")
	}
}

func TestIdempotency_KeyTooLong(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())
	h := mw(okHandler(http.StatusOK, `{}`))

	key := strings.Repeat("k", IdempotencyKeyMaxLength+1)
	req := authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uuid.New(), key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if fake.calls.begin != 0 {
		t.Errorf("downstream / store touched on oversized key: %+v", fake.calls)
	}
}

func TestIdempotency_RequiresAuth(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())
	h := mw(okHandler(http.StatusOK, `{}`))

	req := authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uuid.Nil, "k1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestIdempotency_FreshKeyProceedsAndCachesResponse(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())

	calls := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "v")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	uid := uuid.New()
	body := []byte(`{"name":"alice"}`)

	req := authedRequest(http.MethodPost, "/v1/users", body, uid, "k1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if calls != 1 {
		t.Fatalf("downstream calls = %d, want 1", calls)
	}
	if rr.Code != http.StatusCreated {
		t.Errorf("first call status = %d, want 201", rr.Code)
	}
	if rr.Body.String() != `{"ok":true}` {
		t.Errorf("first call body = %q", rr.Body.String())
	}
	if rr.Header().Get("X-Custom") != "v" {
		t.Errorf("first call X-Custom not propagated")
	}
	if fake.calls.complete != 1 {
		t.Errorf("Complete calls = %d, want 1", fake.calls.complete)
	}

	req2 := authedRequest(http.MethodPost, "/v1/users", body, uid, "k1")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)

	if calls != 1 {
		t.Errorf("downstream called again on replay (calls=%d)", calls)
	}
	if rr2.Code != http.StatusCreated {
		t.Errorf("replay status = %d, want 201", rr2.Code)
	}
	if rr2.Body.String() != `{"ok":true}` {
		t.Errorf("replay body = %q", rr2.Body.String())
	}
	if rr2.Header().Get("X-Custom") != "v" {
		t.Errorf("replay X-Custom not restored")
	}
}

func TestIdempotency_DifferentBodyMismatch(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())
	h := mw(okHandler(http.StatusCreated, `{"ok":true}`))

	uid := uuid.New()

	req1 := authedRequest(http.MethodPost, "/v1/users", []byte(`{"a":1}`), uid, "k1")
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rr1.Code)
	}

	req2 := authedRequest(http.MethodPost, "/v1/users", []byte(`{"a":2}`), uid, "k1")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusConflict {
		t.Errorf("mismatch status = %d, want 409", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "idempotency_key_mismatch") {
		t.Errorf("body = %q, want code idempotency_key_mismatch", rr2.Body.String())
	}
}

func TestIdempotency_DifferentUserMismatch(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())
	h := mw(okHandler(http.StatusCreated, `{"ok":true}`))

	body := []byte(`{"a":1}`)

	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, authedRequest(http.MethodPost, "/v1/users", body, uuid.New(), "k1"))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rr1.Code)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedRequest(http.MethodPost, "/v1/users", body, uuid.New(), "k1"))
	if rr2.Code != http.StatusConflict {
		t.Errorf("cross-user status = %d, want 409", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "idempotency_key_mismatch") {
		t.Errorf("body = %q", rr2.Body.String())
	}
}

func TestIdempotency_ConcurrentInFlightConflict(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())

	uid := uuid.New()

	hold := make(chan struct{})
	release := make(chan struct{})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(hold)
		<-release
		w.WriteHeader(http.StatusCreated)
	}))

	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uid, "k1"))
	}()

	<-hold
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uid, "k1"))
	close(release)

	if rr.Code != http.StatusConflict {
		t.Errorf("concurrent retry status = %d, want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"conflict"`) {
		t.Errorf("body = %q, want code conflict", rr.Body.String())
	}
}

func TestIdempotency_NonSuccessResponseNotCached(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())

	calls := 0
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))

	uid := uuid.New()
	body := []byte(`{"a":1}`)

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedRequest(http.MethodPost, "/v1/users", body, uid, "k1"))
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("attempt %d status = %d, want 500", i+1, rr.Code)
		}
	}
	if calls != 2 {
		t.Errorf("downstream calls = %d, want 2 (no caching of 5xx)", calls)
	}
	if fake.calls.delete < 2 {
		t.Errorf("delete calls = %d, want >= 2", fake.calls.delete)
	}
}

func TestBeginUsesShortLease(t *testing.T) {
	fake := newFakeStore()
	row, action, err := tryBegin(context.Background(), fake, "k2", uuid.New(), "POST", "/v1/vms", []byte("b"))
	if err != nil || action != actionProceed {
		t.Fatalf("tryBegin = %v, %v", action, err)
	}
	lease := time.Until(row.ExpiresAt)
	if lease > IdempotencyInFlightLease+time.Second || lease < time.Second {
		t.Errorf("in_flight lease = %s, want ~%s (not the 24h TTL)", lease, IdempotencyInFlightLease)
	}
}

func TestAcquireReclaimsStaleInFlight(t *testing.T) {
	key := "k1"
	past := time.Now().Add(-time.Minute)
	fake := newFakeStore()
	fake.rows[key] = store.IdempotencyKey{Key: key, State: "in_flight", ExpiresAt: past}

	_, action, err := acquireKey(context.Background(), fake, key, uuid.New(), "POST", "/v1/vms", []byte("body"))
	if err != nil {
		t.Fatalf("acquireKey: %v", err)
	}
	if action != actionProceed {
		t.Errorf("action = %v, want actionProceed (stale in_flight reclaimed)", action)
	}
}

func TestReclaimUsesShortLease(t *testing.T) {
	key := "k-reclaim"
	past := time.Now().Add(-time.Minute)
	fake := newFakeStore()
	fake.rows[key] = store.IdempotencyKey{Key: key, State: "in_flight", ExpiresAt: past}

	row, action, err := acquireKey(context.Background(), fake, key, uuid.New(), "POST", "/v1/vms", []byte("body"))
	if err != nil {
		t.Fatalf("acquireKey: %v", err)
	}
	if action != actionProceed {
		t.Fatalf("action = %v, want actionProceed (reclaimed)", action)
	}
	lease := time.Until(row.ExpiresAt)
	if lease > IdempotencyInFlightLease+time.Second || lease < time.Second {
		t.Errorf("reclaimed in_flight lease = %s, want ~%s (the short lease, not 24h)", lease, IdempotencyInFlightLease)
	}
}

func TestCompleteExtendsLease(t *testing.T) {
	key := "k-complete"
	fake := newFakeStore()

	// Begin stamps the short in_flight lease.
	begun, action, err := tryBegin(context.Background(), fake, key, uuid.New(), "POST", "/v1/vms", []byte("body"))
	if err != nil || action != actionProceed {
		t.Fatalf("tryBegin = %v, %v", action, err)
	}
	if lease := time.Until(begun.ExpiresAt); lease > IdempotencyInFlightLease+time.Second {
		t.Fatalf("begin lease = %s, want short ~%s", lease, IdempotencyInFlightLease)
	}

	// Completing the row extends the lease to the full 24h TTL, exactly as
	// finalizeKey does on a 2xx outcome.
	status := int32(http.StatusCreated)
	if err := fake.CompleteIdempotencyKey(context.Background(), store.CompleteIdempotencyKeyParams{
		ResponseStatus: &status,
		ExpiresAt:      time.Now().Add(IdempotencyTTL),
		Key:            key,
	}); err != nil {
		t.Fatalf("CompleteIdempotencyKey: %v", err)
	}

	completed, err := fake.GetIdempotencyKey(context.Background(), key)
	if err != nil {
		t.Fatalf("GetIdempotencyKey: %v", err)
	}
	lease := time.Until(completed.ExpiresAt)
	if lease < IdempotencyTTL-time.Minute || lease > IdempotencyTTL+time.Second {
		t.Errorf("completed lease = %s, want ~%s (the full 24h TTL)", lease, IdempotencyTTL)
	}
}

func TestIdempotency_StorageErrorReturns500(t *testing.T) {
	fake := newFakeStore()
	fake.getErr = errors.New("db down")
	mw := Idempotency(fake, discardLog())
	h := mw(okHandler(http.StatusOK, `{}`))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uuid.New(), "k1"))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}
