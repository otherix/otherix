// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
)

// TestTimeoutGuardsLateIdempotencyFlush drives the real
// Timeout -> Idempotency -> slow-handler sequence to pin the N5 fix: the
// timeout goroutine and the late idempotency flush must not both write to
// the same ResponseWriter unsynchronised.
//
// The handler blocks past the deadline, then writes a 2xx body into the
// idempotency recorder. The Timeout parent writes a 503 to w and returns
// without joining the child; the detached child eventually finishes and
// Idempotency calls rec.flush(w) on the SAME w the parent already wrote.
// Without the guarded writer this is a data race on the underlying
// recorder (caught under -race) and a write-after-503 that corrupts the
// response (superfluous WriteHeader / interleaved body). With the guard
// the late flush is a no-op and the client keeps the clean 503.
//
// Run with -race.
func TestTimeoutGuardsLateIdempotencyFlush(t *testing.T) {
	fake := newFakeStore()
	idem := Idempotency(fake, discardLog())

	// handlerReturned closes after the slow handler has finished writing
	// into the idempotency recorder, i.e. just before Idempotency runs
	// finalizeKey + rec.flush(w). The detached child then performs the
	// late write that the fix must neutralise.
	handlerReturned := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the deadline fires so the parent has already written
		// the 503, then write a late 2xx that races the flush.
		<-r.Context().Done()
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"late":true}`))
		close(handlerReturned)
	})

	// quiescing recorder: it is NOT internally serialised (so -race can
	// surface the genuine middleware bug), but it tracks the time of the
	// last write so the test can wait for the detached flush to settle.
	spy := &quiescingRecorder{ResponseRecorder: httptest.NewRecorder()}

	h := Timeout(10 * time.Millisecond)(idem(slow))
	req := authedRequest(http.MethodPost, "/v1/users", []byte(`{}`), uuid.New(), "k1")

	h.ServeHTTP(spy, req)

	// Join the handler body, then wait for the late flush to run (or for the
	// guarded writer to swallow it) before reading the recorder.
	<-handlerReturned
	spy.waitQuiescent(t)

	if spy.code() != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (late 2xx must not overwrite the timeout)", spy.code())
	}

	var body response.ErrorBody
	if err := json.Unmarshal(spy.body(), &body); err != nil {
		t.Fatalf("decode body: %v (raw=%q) - late write likely corrupted the 503", err, spy.body())
	}
	if body.Error.Code != response.CodeRequestTimeout {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeRequestTimeout)
	}
}

// quiescingRecorder wraps an httptest.ResponseRecorder and timestamps the
// last write. It deliberately does NOT serialise the embedded recorder:
// the test relies on -race to catch the timeout goroutine and the detached
// idempotency flush writing the recorder concurrently. The mutex guards
// only the lastWrite timestamp, never the recorder fields.
type quiescingRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	lastWrite time.Time
}

func (q *quiescingRecorder) touch() {
	q.mu.Lock()
	q.lastWrite = time.Now()
	q.mu.Unlock()
}

func (q *quiescingRecorder) WriteHeader(status int) {
	q.touch()
	q.ResponseRecorder.WriteHeader(status)
}

func (q *quiescingRecorder) Write(b []byte) (int, error) {
	q.touch()
	return q.ResponseRecorder.Write(b)
}

// waitQuiescent blocks until no write has landed for a short settle window,
// bounding the wait so the fixed (no late write) path is not stalled.
func (q *quiescingRecorder) waitQuiescent(t *testing.T) {
	t.Helper()
	const settle = 60 * time.Millisecond
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		idle := time.Since(q.lastWrite)
		q.mu.Unlock()
		if idle >= settle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (q *quiescingRecorder) code() int {
	return q.Code
}

func (q *quiescingRecorder) body() []byte {
	return q.Body.Bytes()
}
