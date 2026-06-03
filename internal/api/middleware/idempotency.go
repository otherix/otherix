// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// HeaderIdempotencyKey is the request header carrying a client-supplied
// idempotency key.
const HeaderIdempotencyKey = "Idempotency-Key"

// IdempotencyKeyMaxLength bounds the accepted key length and is also
// declared in `components/parameters/IdempotencyKey` of the OpenAPI spec.
const IdempotencyKeyMaxLength = 255

// IdempotencyTTL is the lifetime of a stored idempotency record. The
// value is 24 hours.
const IdempotencyTTL = 24 * time.Hour

// IdempotencyInFlightLease bounds how long an in_flight record blocks a retry.
// It must exceed the maximum real request duration (the server WriteTimeout, 30s
// default) so a live request is never reclaimed, while a crashed in_flight is
// reclaimable far sooner than the 24h completed-record TTL (IdempotencyTTL).
const IdempotencyInFlightLease = 2 * time.Minute

// IdempotencyStore is the narrow contract Idempotency requires from the
// data layer. *store.Queries implements it; tests substitute an
// in-memory stub.
type IdempotencyStore interface {
	GetIdempotencyKey(ctx context.Context, key string) (store.IdempotencyKey, error)
	BeginIdempotencyKey(ctx context.Context, arg store.BeginIdempotencyKeyParams) (store.IdempotencyKey, error)
	ReclaimIdempotencyKey(ctx context.Context, arg store.ReclaimIdempotencyKeyParams) (store.IdempotencyKey, error)
	CompleteIdempotencyKey(ctx context.Context, arg store.CompleteIdempotencyKeyParams) error
	DeleteIdempotencyKey(ctx context.Context, key string) error
}

// hopByHopHeaders are not replayed verbatim — they are connection or
// transport metadata that the new response must compute itself.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Content-Length":      {},
	"Date":                {},
	"Keep-Alive":          {},
	"Transfer-Encoding":   {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Upgrade":             {},
}

// Idempotency returns a middleware that implements the Idempotency-Key
// contract. The header is optional and only honoured for mutating
// methods (POST, PUT, PATCH, DELETE).
//
// On a fresh key the middleware inserts an in_flight row, runs the
// downstream handler buffered, and on a 2xx outcome stores the response
// for replay. A non-2xx outcome deletes the in_flight row so the client
// may retry with the same key.
//
// On a hit the middleware enforces (user_id, method, path, body-hash)
// equality. Any mismatch produces 409 idempotency_key_mismatch — leaking
// "this key was used by someone else" is acceptable because the key was
// supplied by the client. A row in_flight by an earlier caller produces
// 409 conflict so the client can back off and retry.
//
// Idempotency MUST run after Authn — the row is scoped per user.
func Idempotency(s IdempotencyStore, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isMutatingMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(HeaderIdempotencyKey)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > IdempotencyKeyMaxLength {
				response.WriteError(w, r, http.StatusBadRequest,
					response.CodeValidationFailed,
					"idempotency key exceeds maximum length",
					map[string]any{"max_length": IdempotencyKeyMaxLength})
				return
			}

			user := auth.UserFromContext(r.Context())
			if user == nil {
				response.WriteError(w, r, http.StatusUnauthorized,
					response.CodeUnauthenticated,
					"idempotency key requires authentication", nil)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				response.WriteError(w, r, http.StatusBadRequest,
					response.CodeValidationFailed,
					"failed to read request body", nil)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := sha256.Sum256(body)

			row, action, err := acquireKey(r.Context(), s, key, user.ID, r.Method, r.URL.Path, hash[:])
			if err != nil {
				log.ErrorContext(r.Context(), "idempotency acquire failed",
					slog.String("key", key),
					slog.String("user_id", user.ID.String()),
					slog.String("error", err.Error()))
				response.WriteError(w, r, http.StatusInternalServerError,
					response.CodeInternal, "idempotency check failed", nil)
				return
			}

			switch action {
			case actionReplay:
				replayCachedResponse(w, row)
				return
			case actionMismatch:
				response.WriteError(w, r, http.StatusConflict,
					response.CodeIdempotencyMismatch,
					"idempotency key reused with different request", nil)
				return
			case actionInFlight:
				response.WriteError(w, r, http.StatusConflict,
					response.CodeConflict,
					"a request with this idempotency key is already in flight",
					map[string]any{"retry_after_seconds": 1})
				return
			case actionProceed:
				rec := newRecorder()
				next.ServeHTTP(rec, r)
				// Settle the durable idempotency row (Complete on 2xx,
				// Delete on non-2xx) BEFORE the client sees any bytes. If
				// the process crashes between here and the flush, the
				// client never received a 2xx, so its retry replays as a
				// first attempt against the reclaimable in_flight row.
				finalizeKey(r.Context(), s, key, rec, log)
				rec.flush(w)
			}
		})
	}
}

// idemAction enumerates the outcomes of the acquire step. acquireKey
// reads (and possibly writes) the idempotency row and reduces the
// state machine to one of these outcomes for the middleware body.
type idemAction int

const (
	actionProceed  idemAction = iota // Run the handler buffered, then Complete or Delete.
	actionReplay                     // The cached response is replayed verbatim.
	actionMismatch                   // 409 idempotency_key_mismatch.
	actionInFlight                   // 409 conflict — concurrent request still running.
)

// idempotencyNotFound reports whether err is the store's "row absent" sentinel.
// Begin-conflict and reclaim-no-op both map to store.ErrNotFound, so the
// middleware treats them uniformly.
func idempotencyNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

func acquireKey(ctx context.Context, s IdempotencyStore, key string, userID uuid.UUID, method, path string, hash []byte) (store.IdempotencyKey, idemAction, error) {
	row, err := s.GetIdempotencyKey(ctx, key)
	if idempotencyNotFound(err) {
		return tryBegin(ctx, s, key, userID, method, path, hash)
	}
	if err != nil {
		return store.IdempotencyKey{}, 0, err
	}

	if time.Now().After(row.ExpiresAt) {
		uid := userIDPtr(userID)
		row, err = s.ReclaimIdempotencyKey(ctx, store.ReclaimIdempotencyKeyParams{
			UserID:        uid,
			RequestMethod: method,
			RequestPath:   path,
			RequestHash:   hash,
			ExpiresAt:     time.Now().Add(IdempotencyInFlightLease),
			Key:           key,
		})
		if idempotencyNotFound(err) {
			row, err = s.GetIdempotencyKey(ctx, key)
			if err != nil {
				return store.IdempotencyKey{}, 0, err
			}
			return row, determineAction(row, userID, method, path, hash), nil
		}
		if err != nil {
			return store.IdempotencyKey{}, 0, err
		}
		return row, actionProceed, nil
	}

	return row, determineAction(row, userID, method, path, hash), nil
}

func tryBegin(ctx context.Context, s IdempotencyStore, key string, userID uuid.UUID, method, path string, hash []byte) (store.IdempotencyKey, idemAction, error) {
	uid := userIDPtr(userID)
	row, err := s.BeginIdempotencyKey(ctx, store.BeginIdempotencyKeyParams{
		Key:           key,
		UserID:        uid,
		RequestMethod: method,
		RequestPath:   path,
		RequestHash:   hash,
		ExpiresAt:     time.Now().Add(IdempotencyInFlightLease),
	})
	if err == nil {
		return row, actionProceed, nil
	}
	if !idempotencyNotFound(err) {
		return store.IdempotencyKey{}, 0, err
	}
	row, err = s.GetIdempotencyKey(ctx, key)
	if err != nil {
		return store.IdempotencyKey{}, 0, err
	}
	return row, determineAction(row, userID, method, path, hash), nil
}

func determineAction(row store.IdempotencyKey, userID uuid.UUID, method, path string, hash []byte) idemAction {
	if row.UserID == nil || *row.UserID != userID {
		return actionMismatch
	}
	if row.RequestMethod != method || row.RequestPath != path {
		return actionMismatch
	}
	if !bytes.Equal(row.RequestHash, hash) {
		return actionMismatch
	}
	switch row.State {
	case "in_flight":
		return actionInFlight
	case "completed":
		return actionReplay
	default:
		return actionMismatch
	}
}

func finalizeKey(ctx context.Context, s IdempotencyStore, key string, rec *recorder, log *slog.Logger) {
	if rec.statusCode >= 200 && rec.statusCode < 300 {
		hdr, err := encodeHeaders(rec.headers)
		if err != nil {
			log.ErrorContext(ctx, "idempotency encode headers failed",
				slog.String("key", key),
				slog.String("error", err.Error()))
			return
		}
		status := int32(rec.statusCode) //nolint:gosec // status is bounded by net/http
		if err := s.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
			ResponseStatus:  &status,
			ResponseHeaders: hdr,
			ResponseBody:    rec.body.Bytes(),
			ExpiresAt:       time.Now().Add(IdempotencyTTL),
			Key:             key,
		}); err != nil {
			log.ErrorContext(ctx, "idempotency complete failed",
				slog.String("key", key),
				slog.String("error", err.Error()))
		}
		return
	}
	if err := s.DeleteIdempotencyKey(ctx, key); err != nil {
		log.ErrorContext(ctx, "idempotency delete failed",
			slog.String("key", key),
			slog.String("error", err.Error()))
	}
}

func replayCachedResponse(w http.ResponseWriter, row store.IdempotencyKey) {
	hdrs, err := decodeHeaders(row.ResponseHeaders)
	if err == nil {
		for k, vs := range hdrs {
			if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(k)]; skip {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	status := http.StatusOK
	if row.ResponseStatus != nil {
		status = int(*row.ResponseStatus)
	}
	w.WriteHeader(status)
	_, _ = w.Write(row.ResponseBody)
}

func encodeHeaders(h http.Header) ([]byte, error) {
	if len(h) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(h)
}

func decodeHeaders(b []byte) (http.Header, error) {
	if len(b) == 0 {
		return http.Header{}, nil
	}
	var out http.Header
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func isMutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// userIDPtr returns a pointer to a copy of id, suitable for the
// sqlc-generated nullable UUID parameter.
func userIDPtr(id uuid.UUID) *uuid.UUID {
	v := id
	return &v
}

// recorder fully buffers the response written by a downstream handler.
// It never touches a real ResponseWriter while the handler runs: the
// status, headers, and body are captured in memory so the middleware can
// persist the durable idempotency row before any byte reaches the client.
// The buffered response is emitted later via flush.
type recorder struct {
	statusCode  int
	body        *bytes.Buffer
	headers     http.Header
	wroteHeader bool
}

func newRecorder() *recorder {
	return &recorder{
		statusCode: http.StatusOK,
		body:       &bytes.Buffer{},
		headers:    http.Header{},
	}
}

// Header returns the recorder's own header map. The downstream handler
// writes into it; flush copies entries onto the real ResponseWriter and
// finalizeKey snapshots them for the cache.
func (r *recorder) Header() http.Header {
	return r.headers
}

// WriteHeader captures the status code only. Nothing is forwarded to a
// real writer here — the response stays buffered until flush.
func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.statusCode = status
}

// Write buffers the body. An implicit 200 status is materialised on the
// first Write call to mirror net/http's default. Nothing is forwarded to
// a real writer here — the response stays buffered until flush.
func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(b)
}

// flush emits the buffered status, headers, and body to w. It runs only
// after the durable idempotency row is settled, so the client never sees
// bytes before the state is committed. Hop-by-hop headers are filtered
// for parity with replayCachedResponse.
func (r *recorder) flush(w http.ResponseWriter) {
	dst := w.Header()
	for k, vs := range r.headers {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}
