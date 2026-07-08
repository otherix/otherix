// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

// idempMapKey mirrors the store's per-user key scoping in the in-memory fake:
// rows are keyed on (user_id, key) so two users using the same key string do
// not collide.
func idempMapKey(userID uuid.UUID, key string) string {
	return userID.String() + "/" + key
}

func (f *fakeIdempStore) GetIdempotencyKey(_ context.Context, userID uuid.UUID, key string) (store.IdempotencyKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.get++
	if f.getErr != nil {
		return store.IdempotencyKey{}, f.getErr
	}
	row, ok := f.rows[idempMapKey(userID, key)]
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
	if arg.UserID == nil {
		return store.IdempotencyKey{}, fmt.Errorf("begin idempotency key requires a user id")
	}
	mk := idempMapKey(*arg.UserID, arg.Key)
	if _, exists := f.rows[mk]; exists {
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
	f.rows[mk] = row
	return row, nil
}

func (f *fakeIdempStore) ReclaimIdempotencyKey(_ context.Context, arg store.ReclaimIdempotencyKeyParams) (store.IdempotencyKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.reclaim++
	if arg.UserID == nil {
		return store.IdempotencyKey{}, fmt.Errorf("reclaim idempotency key requires a user id")
	}
	mk := idempMapKey(*arg.UserID, arg.Key)
	row, ok := f.rows[mk]
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
	f.rows[mk] = row
	return row, nil
}

func (f *fakeIdempStore) CompleteIdempotencyKey(_ context.Context, arg store.CompleteIdempotencyKeyParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.complete++
	if arg.UserID == nil {
		return fmt.Errorf("complete idempotency key requires a user id")
	}
	mk := idempMapKey(*arg.UserID, arg.Key)
	row, ok := f.rows[mk]
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
	f.rows[mk] = row
	return nil
}

func (f *fakeIdempStore) DeleteIdempotencyKey(_ context.Context, userID uuid.UUID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.delete++
	mk := idempMapKey(userID, key)
	if row, ok := f.rows[mk]; ok && row.State == "in_flight" {
		delete(f.rows, mk)
	}
	return nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ctxAwareIdempStore fails the settle writes (Complete / Delete) when their
// context is already cancelled, modelling a real etcd client that aborts a
// write on a dead request context (client disconnect or Timeout deadline).
type ctxAwareIdempStore struct {
	*fakeIdempStore
}

func (c *ctxAwareIdempStore) CompleteIdempotencyKey(ctx context.Context, arg store.CompleteIdempotencyKeyParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.fakeIdempStore.CompleteIdempotencyKey(ctx, arg)
}

func (c *ctxAwareIdempStore) DeleteIdempotencyKey(ctx context.Context, userID uuid.UUID, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.fakeIdempStore.DeleteIdempotencyKey(ctx, userID, key)
}

// TestIdempotency_SettlesRowAfterRequestCancel proves the durable idempotency
// row is committed even when the request context is already cancelled by the
// time the handler returns (a client disconnect, or the Timeout middleware
// firing while Idempotency runs inside its detached goroutine). The handler has
// already run - its side effects happened - so finalizeKey must settle on a
// context detached from the request's cancellation. Otherwise the row wedges
// in_flight: the response is never recorded and a later retry re-runs the
// operation, breaking the idempotency guarantee.
func TestIdempotency_SettlesRowAfterRequestCancel(t *testing.T) {
	st := &ctxAwareIdempStore{fakeIdempStore: newFakeStore()}
	uid := uuid.New()
	const key = "cancel-key"

	var reqCancel context.CancelFunc
	h := Idempotency(st, discardLog())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		// Cancel the request context before finalizeKey runs, as a client
		// disconnect / deadline would.
		reqCancel()
	}))

	req := authedRequest(http.MethodPost, "/v1/things", []byte(`{}`), uid, key)
	ctx, cancel := context.WithCancel(req.Context())
	reqCancel = cancel
	defer cancel()
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got, err := st.GetIdempotencyKey(context.Background(), uid, key)
	if err != nil {
		t.Fatalf("GetIdempotencyKey: %v", err)
	}
	if got.State != "completed" {
		t.Errorf("row state = %q, want completed (settle must survive a cancelled request context)", got.State)
	}
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

// observingWriter wraps an httptest.ResponseRecorder and runs a callback
// the moment any byte (header or body) reaches the underlying writer. It
// lets a test assert *when* the client first observes a response relative
// to other events (e.g. CompleteIdempotencyKey).
type observingWriter struct {
	*httptest.ResponseRecorder
	onFirstWrite func()
	wrote        bool
}

func (o *observingWriter) note() {
	if !o.wrote {
		o.wrote = true
		if o.onFirstWrite != nil {
			o.onFirstWrite()
		}
	}
}

func (o *observingWriter) WriteHeader(status int) {
	o.note()
	o.ResponseRecorder.WriteHeader(status)
}

func (o *observingWriter) Write(b []byte) (int, error) {
	o.note()
	return o.ResponseRecorder.Write(b)
}

// completeSpyStore records, at the instant CompleteIdempotencyKey is
// invoked, whether the client has already seen any response bytes.
type completeSpyStore struct {
	*fakeIdempStore
	clientWroteAtComplete bool
	clientWrote           func() bool
}

func (c *completeSpyStore) CompleteIdempotencyKey(ctx context.Context, arg store.CompleteIdempotencyKeyParams) error {
	if c.clientWrote != nil {
		c.clientWroteAtComplete = c.clientWrote()
	}
	return c.fakeIdempStore.CompleteIdempotencyKey(ctx, arg)
}

func TestIdempotencyDoesNotFlushBeforeCommit(t *testing.T) {
	spy := &completeSpyStore{fakeIdempStore: newFakeStore()}
	mw := Idempotency(spy, discardLog())

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	rr := &observingWriter{ResponseRecorder: httptest.NewRecorder()}
	spy.clientWrote = func() bool { return rr.wrote }
	rr.onFirstWrite = func() {
		if spy.calls.complete == 0 {
			t.Errorf("client observed response bytes before CompleteIdempotencyKey ran")
		}
	}

	h.ServeHTTP(rr, authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uuid.New(), "k1"))

	if spy.clientWroteAtComplete {
		t.Errorf("client had already seen response bytes when Complete was invoked")
	}
	if !rr.wrote {
		t.Errorf("client never received the buffered response")
	}
}

func TestIdempotencyFlushesAfterCommit(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "v")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uuid.New(), "k1"))

	if fake.calls.complete != 1 {
		t.Fatalf("Complete calls = %d, want 1", fake.calls.complete)
	}
	if rr.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rr.Code)
	}
	if rr.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q, want %q", rr.Body.String(), `{"ok":true}`)
	}
	if rr.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rr.Header().Get("Content-Type"))
	}
	if rr.Header().Get("X-Custom") != "v" {
		t.Errorf("X-Custom = %q, want v", rr.Header().Get("X-Custom"))
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

// TestIdempotency_DifferentUsersDoNotCollide asserts that two different users
// using the SAME key string get independent namespaces: the second user's
// request runs on its own row instead of colliding with the first user's row
// (the old global-key scope returned 409 here, letting a user
// squat a key string to grief others).
func TestIdempotency_DifferentUsersDoNotCollide(t *testing.T) {
	fake := newFakeStore()
	mw := Idempotency(fake, discardLog())
	h := mw(okHandler(http.StatusCreated, `{"ok":true}`))

	body := []byte(`{"a":1}`)

	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, authedRequest(http.MethodPost, "/v1/users", body, uuid.New(), "k1"))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first user status = %d, want 201", rr1.Code)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, authedRequest(http.MethodPost, "/v1/users", body, uuid.New(), "k1"))
	if rr2.Code != http.StatusCreated {
		t.Errorf("second user status = %d, want 201 (no cross-user collision)", rr2.Code)
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
	userID := uuid.New()
	past := time.Now().Add(-time.Minute)
	fake := newFakeStore()
	fake.rows[idempMapKey(userID, key)] = store.IdempotencyKey{Key: key, UserID: &userID, State: "in_flight", ExpiresAt: past}

	_, action, err := acquireKey(context.Background(), fake, key, userID, "POST", "/v1/vms", []byte("body"))
	if err != nil {
		t.Fatalf("acquireKey: %v", err)
	}
	if action != actionProceed {
		t.Errorf("action = %v, want actionProceed (stale in_flight reclaimed)", action)
	}
}

func TestReclaimUsesShortLease(t *testing.T) {
	key := "k-reclaim"
	userID := uuid.New()
	past := time.Now().Add(-time.Minute)
	fake := newFakeStore()
	fake.rows[idempMapKey(userID, key)] = store.IdempotencyKey{Key: key, UserID: &userID, State: "in_flight", ExpiresAt: past}

	row, action, err := acquireKey(context.Background(), fake, key, userID, "POST", "/v1/vms", []byte("body"))
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
	userID := uuid.New()
	fake := newFakeStore()

	// Begin stamps the short in_flight lease.
	begun, action, err := tryBegin(context.Background(), fake, key, userID, "POST", "/v1/vms", []byte("body"))
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
		UserID:         &userID,
		ResponseStatus: &status,
		ExpiresAt:      time.Now().Add(IdempotencyTTL),
		Key:            key,
	}); err != nil {
		t.Fatalf("CompleteIdempotencyKey: %v", err)
	}

	completed, err := fake.GetIdempotencyKey(context.Background(), userID, key)
	if err != nil {
		t.Fatalf("GetIdempotencyKey: %v", err)
	}
	lease := time.Until(completed.ExpiresAt)
	if lease < IdempotencyTTL-time.Minute || lease > IdempotencyTTL+time.Second {
		t.Errorf("completed lease = %s, want ~%s (the full 24h TTL)", lease, IdempotencyTTL)
	}
}

// TestIdempotency_ExposesDescriptorOnProceed proves the middleware places the
// idempotency descriptor (key + sha256(body)) on the request context for the
// downstream handler on the actionProceed path, and leaves it absent when the
// request is non-mutating or carries no key.
func TestIdempotency_ExposesDescriptorOnProceed(t *testing.T) {
	uid := uuid.New()
	body := []byte(`{"name":"alice"}`)
	wantHash := sha256.Sum256(body)

	tests := []struct {
		name    string
		method  string
		key     string
		body    []byte
		wantKey string // empty means descriptor must be nil
	}{
		{name: "mutating with key", method: http.MethodPost, key: "k1", body: body, wantKey: "k1"},
		{name: "non-mutating with key", method: http.MethodGet, key: "k1", body: nil, wantKey: ""},
		{name: "mutating without key", method: http.MethodPost, key: "", body: body, wantKey: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStore()
			var got *IdempotencyDescriptor
			h := Idempotency(fake, discardLog())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = IdempotencyFromContext(r.Context())
				w.WriteHeader(http.StatusCreated)
			}))

			req := authedRequest(tc.method, "/v1/things", tc.body, uid, tc.key)
			h.ServeHTTP(httptest.NewRecorder(), req)

			if tc.wantKey == "" {
				if got != nil {
					t.Fatalf("IdempotencyFromContext = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("IdempotencyFromContext = nil, want descriptor")
			}
			if got.Key != tc.wantKey {
				t.Errorf("descriptor Key = %q, want %q", got.Key, tc.wantKey)
			}
			if !bytes.Equal(got.Hash, wantHash[:]) {
				t.Errorf("descriptor Hash = %x, want %x", got.Hash, wantHash[:])
			}
		})
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
