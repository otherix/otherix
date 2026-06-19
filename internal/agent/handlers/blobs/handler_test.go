// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobs

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// serveSpy records the last Serve call and returns scripted results. It stands
// in for the production blobpeer serve manager (which opens a real listener);
// the spy lets the handler test assert the wire translation without binding a
// port.
type serveSpy struct {
	calls     int
	digest    string
	token     string
	consumer  string
	endpoint  string
	expiresAt string
	returnErr error
}

func (s *serveSpy) Serve(digest, token, consumerNodeID string) (string, string, error) {
	s.calls++
	s.digest = digest
	s.token = token
	s.consumer = consumerNodeID
	if s.returnErr != nil {
		return "", "", s.returnErr
	}
	return s.endpoint, s.expiresAt, nil
}

// pullSpy records the last Pull call and returns a scripted task id / error. It
// stands in for the production puller adapter (which runs blobpeer.Pull as an
// agent task).
type pullSpy struct {
	calls          int
	digest         string
	token          string
	holderEndpoint string
	holderIdentity string
	expectedSize   int64
	taskID         string
	returnErr      error
}

func (s *pullSpy) Pull(digest, token, holderEndpoint, holderIdentity string, expectedSize int64) (string, error) {
	s.calls++
	s.digest = digest
	s.token = token
	s.holderEndpoint = holderEndpoint
	s.holderIdentity = holderIdentity
	s.expectedSize = expectedSize
	if s.returnErr != nil {
		return "", s.returnErr
	}
	return s.taskID, nil
}

// stopSpy records the tokens StopServe was driven with. It stands in for the
// production serve manager (whose StopServe tears a real listener down).
type stopSpy struct {
	tokens []string
}

func (s *stopSpy) StopServe(token string) {
	s.tokens = append(s.tokens, token)
}

func mountTestRouter(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Route("/blobs", h.Mount)
	})
	return r
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestServe_HappyPath(t *testing.T) {
	digest := strings.Repeat("a", 64)
	consumer := uuid.New().String()
	srv := &serveSpy{
		endpoint:  "https://10.0.0.2:49252",
		expiresAt: "2026-06-18T12:00:00Z",
	}
	h := New(srv, &pullSpy{}, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"digest":           digest,
		"token":            "tok-123",
		"consumer_node_id": consumer,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/serve", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if srv.calls != 1 {
		t.Fatalf("Serve calls = %d, want 1", srv.calls)
	}
	if srv.digest != digest || srv.token != "tok-123" || srv.consumer != consumer {
		t.Errorf("Serve(%q,%q,%q), want (%q,%q,%q)", srv.digest, srv.token, srv.consumer, digest, "tok-123", consumer)
	}
	var resp struct {
		ServeEndpoint string `json:"serve_endpoint"`
		ExpiresAt     string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if resp.ServeEndpoint != srv.endpoint {
		t.Errorf("serve_endpoint = %q, want %q", resp.ServeEndpoint, srv.endpoint)
	}
	if resp.ExpiresAt != srv.expiresAt {
		t.Errorf("expires_at = %q, want %q", resp.ExpiresAt, srv.expiresAt)
	}
}

func TestServe_BadRequestOnMissingDigest(t *testing.T) {
	h := New(&serveSpy{}, &pullSpy{}, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"token":            "tok-123",
		"consumer_node_id": uuid.New().String(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/serve", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestServe_BadRequestOnMissingConsumerNodeID(t *testing.T) {
	srv := &serveSpy{}
	h := New(srv, &pullSpy{}, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"digest": strings.Repeat("a", 64),
		"token":  "tok-123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/serve", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if srv.calls != 0 {
		t.Errorf("Serve calls = %d, want 0 (handler must reject before dispatch)", srv.calls)
	}
}

func TestServe_MalformedJSON(t *testing.T) {
	h := New(&serveSpy{}, &pullSpy{}, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/serve", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestServe_InternalErrorFromManager(t *testing.T) {
	srv := &serveSpy{returnErr: errors.New("no free port")}
	h := New(srv, &pullSpy{}, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"digest":           strings.Repeat("b", 64),
		"token":            "tok-123",
		"consumer_node_id": uuid.New().String(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/serve", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestPull_HappyPath(t *testing.T) {
	digest := strings.Repeat("c", 64)
	taskID := uuid.New().String()
	pull := &pullSpy{taskID: taskID}
	h := New(&serveSpy{}, pull, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"digest":          digest,
		"token":           "tok-456",
		"holder_endpoint": "https://10.0.0.3:49252",
		"holder_identity": "node-holder.agents.otherix.local",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/pull", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if pull.calls != 1 {
		t.Fatalf("Pull calls = %d, want 1", pull.calls)
	}
	if pull.digest != digest || pull.token != "tok-456" || pull.holderEndpoint != "https://10.0.0.3:49252" {
		t.Errorf("Pull(%q,%q,%q)", pull.digest, pull.token, pull.holderEndpoint)
	}
	if pull.holderIdentity != "node-holder.agents.otherix.local" {
		t.Errorf("Pull holderIdentity = %q, want %q", pull.holderIdentity, "node-holder.agents.otherix.local")
	}
	var resp asyncAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, rec.Body.String())
	}
	if resp.TaskID != taskID {
		t.Errorf("task_id = %q, want %q", resp.TaskID, taskID)
	}
	switch resp.Status {
	case "pending", "running":
		// expected
	default:
		t.Errorf("status = %q, want pending or running", resp.Status)
	}
	self, ok := resp.Links["self"].(string)
	if !ok || self != "/v1/tasks/"+taskID {
		t.Errorf("links.self = %v, want /v1/tasks/%s", resp.Links["self"], taskID)
	}
}

func TestPull_BadRequestOnMissingHolderEndpoint(t *testing.T) {
	h := New(&serveSpy{}, &pullSpy{}, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"digest": strings.Repeat("d", 64),
		"token":  "tok-456",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/pull", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPull_InternalErrorFromPuller(t *testing.T) {
	pull := &pullSpy{returnErr: errors.New("task store full")}
	h := New(&serveSpy{}, pull, &stopSpy{}, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{
		"digest":          strings.Repeat("e", 64),
		"token":           "tok-456",
		"holder_endpoint": "https://10.0.0.3:49252",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/pull", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestStopServe_HappyPath(t *testing.T) {
	stop := &stopSpy{}
	h := New(&serveSpy{}, &pullSpy{}, stop, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{"token": "tok-789"})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/stop-serve", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if len(stop.tokens) != 1 || stop.tokens[0] != "tok-789" {
		t.Errorf("StopServe tokens = %v, want [tok-789]", stop.tokens)
	}
}

func TestStopServe_BadRequestOnMissingToken(t *testing.T) {
	stop := &stopSpy{}
	h := New(&serveSpy{}, &pullSpy{}, stop, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/stop-serve", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if len(stop.tokens) != 0 {
		t.Errorf("StopServe tokens = %v, want none (handler must reject before dispatch)", stop.tokens)
	}
}

func TestStopServe_MalformedJSON(t *testing.T) {
	stop := &stopSpy{}
	h := New(&serveSpy{}, &pullSpy{}, stop, &deleterSpy{}, discardLogger())
	router := mountTestRouter(h)

	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/stop-serve", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if len(stop.tokens) != 0 {
		t.Errorf("StopServe tokens = %v, want none", stop.tokens)
	}
}
