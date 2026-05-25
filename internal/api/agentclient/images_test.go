// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
)

// imageImportReq composes a minimal ImageImportRequest with a URL
// source. The agent's contract requires either source_url or
// source_path plus expected_checksum_sha256 + format.
func imageImportReq(checksum string) agentapi.ImageImportRequest {
	url := "https://example.test/img.qcow2"
	return agentapi.ImageImportRequest{
		ExpectedChecksumSha256: &checksum,
		Format:                 agentapi.ImageImportRequestFormat("qcow2"),
		SourceURL:              &url,
	}
}

// hexsha returns a deterministic 64-char lowercase hex sha256 seeded
// with b repeated 32 times.
func hexsha(b byte) string {
	out := make([]byte, 64)
	for i := 0; i < 32; i++ {
		hi, lo := b>>4, b&0x0f
		out[i*2] = hexnibble(hi)
		out[i*2+1] = hexnibble(lo)
	}
	return string(out)
}

func hexnibble(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}

func TestPostImageImport_HappyPath(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	wantTaskID := uuid.New()
	poolID := "test-pool"
	idemKey := "idem-" + uuid.NewString()
	checksum := hexsha(0x11)

	var (
		seenIdemKey atomic.Value
		seenCT      atomic.Value
		seenMethod  atomic.Value
		seenBody    atomic.Value
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images", func(w http.ResponseWriter, r *http.Request) {
		seenMethod.Store(r.Method)
		seenIdemKey.Store(r.Header.Get("Idempotency-Key"))
		seenCT.Store(r.Header.Get("Content-Type"))

		var got agentapi.ImageImportRequest
		if err := decodeReqJSON(r, &got); err != nil {
			t.Errorf("decode req: %v", err)
		}
		seenBody.Store(got)

		jsonWrite(t, w, http.StatusAccepted, map[string]any{
			"task_id": wantTaskID,
			"status":  "pending",
			"links":   map[string]any{"self": fmt.Sprintf("/v1/tasks/%s", wantTaskID)},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	gotID, err := cli.PostImageImport(context.Background(), agentURL, poolID, idemKey, imageImportReq(checksum))
	if err != nil {
		t.Fatalf("PostImageImport: %v", err)
	}
	if gotID != wantTaskID {
		t.Errorf("task_id = %s, want %s", gotID, wantTaskID)
	}
	if got, _ := seenMethod.Load().(string); got != http.MethodPost {
		t.Errorf("method = %q, want POST", got)
	}
	if got, _ := seenIdemKey.Load().(string); got != idemKey {
		t.Errorf("Idempotency-Key = %q, want %q", got, idemKey)
	}
	if got, _ := seenCT.Load().(string); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body, _ := seenBody.Load().(agentapi.ImageImportRequest)
	if body.ExpectedChecksumSha256 == nil || *body.ExpectedChecksumSha256 != checksum {
		got := "<nil>"
		if body.ExpectedChecksumSha256 != nil {
			got = *body.ExpectedChecksumSha256
		}
		t.Errorf("body.ExpectedChecksumSha256 = %q, want %q", got, checksum)
	}
	if string(body.Format) != "qcow2" {
		t.Errorf("body.Format = %q, want qcow2", body.Format)
	}
}

func TestPostImageImport_4xxNoRetry(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	poolID := "test-pool"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images", func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "image_checksum_mismatch",
				"message": "fetched bytes do not match expected_checksum_sha256",
			},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	_, err := cli.PostImageImport(context.Background(), agentURL, poolID, "k", imageImportReq(hexsha(0x22)))
	if err == nil {
		t.Fatal("PostImageImport returned nil error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *AgentError", err, err)
	}
	if ae.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", ae.Status)
	}
	if ae.IsRetryable() {
		t.Errorf("IsRetryable() = true, want false (4xx is permanent)")
	}
	if ae.Code != "image_checksum_mismatch" {
		t.Errorf("Code = %q, want %q", ae.Code, "image_checksum_mismatch")
	}
}

func TestPostImageImport_5xxRetryable(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	poolID := "test-pool"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images", func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": "internal", "message": "upstream tarball server is down"},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	_, err := cli.PostImageImport(context.Background(), agentURL, poolID, "k", imageImportReq(hexsha(0x33)))
	if err == nil {
		t.Fatal("PostImageImport returned nil error on 502")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *AgentError", err, err)
	}
	if ae.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want 502", ae.Status)
	}
	if !ae.IsRetryable() {
		t.Errorf("IsRetryable() = false, want true (5xx is transient)")
	}
}

func TestPostImageImport_TLSHandshakeFails(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	cli := newClient(t, dir)

	_, err := cli.PostImageImport(context.Background(), srv.URL, "test-pool", "k", imageImportReq(hexsha(0x44)))
	if err == nil {
		t.Fatal("PostImageImport against untrusted server returned nil error")
	}
	if !containsAny(err.Error(), "tls", "x509", "certificate") {
		t.Errorf("err = %q, want TLS/x509 substring", err.Error())
	}
}

func TestDeleteImage_HappyPath(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	poolID := "test-pool"
	checksum := hexsha(0x55)
	wantPath := "/v1/storage-pools/" + poolID + "/images/" + checksum

	var (
		seenMethod atomic.Value
		seenPath   atomic.Value
		seenIdem   atomic.Value
	)
	mux := http.NewServeMux()
	mux.HandleFunc(wantPath, func(w http.ResponseWriter, r *http.Request) {
		seenMethod.Store(r.Method)
		seenPath.Store(r.URL.Path)
		seenIdem.Store(r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusNoContent)
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	if err := cli.DeleteImage(context.Background(), agentURL, poolID, checksum, "idem-1"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if got, _ := seenMethod.Load().(string); got != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", got)
	}
	if got, _ := seenPath.Load().(string); got != wantPath {
		t.Errorf("path = %q, want %q", got, wantPath)
	}
	if got, _ := seenIdem.Load().(string); got != "idem-1" {
		t.Errorf("Idempotency-Key = %q, want idem-1", got)
	}
}

func TestDeleteImage_404Idempotent(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	poolID := "test-pool"
	checksum := hexsha(0x66)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images/"+checksum, func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "image not present"},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	if err := cli.DeleteImage(context.Background(), agentURL, poolID, checksum, ""); err != nil {
		t.Errorf("DeleteImage: %v, want nil (404 is idempotent)", err)
	}
}

func TestDeleteImage_5xxRetryable(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	poolID := "test-pool"
	checksum := hexsha(0x77)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images/"+checksum, func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"code": "internal", "message": "fs busy"},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	err := cli.DeleteImage(context.Background(), agentURL, poolID, checksum, "")
	if err == nil {
		t.Fatal("DeleteImage returned nil on 503")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *AgentError", err, err)
	}
	if !ae.IsRetryable() {
		t.Errorf("IsRetryable() = false, want true (5xx)")
	}
}

func TestDeleteImage_409NoRetry(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	poolID := "test-pool"
	checksum := hexsha(0x88)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images/"+checksum, func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "image_in_use", "message": "active disk references this image"},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	err := cli.DeleteImage(context.Background(), agentURL, poolID, checksum, "")
	if err == nil {
		t.Fatal("DeleteImage returned nil on 409 image_in_use")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *AgentError", err, err)
	}
	if ae.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", ae.Status)
	}
	if ae.IsRetryable() {
		t.Errorf("IsRetryable() = true, want false (4xx is permanent)")
	}
	if ae.Code != "image_in_use" {
		t.Errorf("Code = %q, want image_in_use", ae.Code)
	}
}

func TestListImages_SinglePage(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	poolID := "test-pool"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("cursor"); got != "" {
			t.Errorf("first request carried cursor = %q, want empty", got)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"checksum_sha256": hexsha(0xaa), "format": "qcow2", "path": "/var/lib/img/aa.qcow2", "size_bytes": 1024},
				{"checksum_sha256": hexsha(0xbb), "format": "qcow2", "path": "/var/lib/img/bb.qcow2", "size_bytes": 2048},
			},
			"meta": map[string]any{"next_cursor": nil},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	got, err := cli.ListImages(context.Background(), agentURL, poolID)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].SizeBytes != 1024 || got[1].SizeBytes != 2048 {
		t.Errorf("size_bytes = %d/%d, want 1024/2048", got[0].SizeBytes, got[1].SizeBytes)
	}
}

func TestListImages_Paginated3Pages(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	poolID := "test-pool"

	var (
		hits        atomic.Int32
		seenCursors []string
	)
	cursor1 := "cur sor 1" // include space to verify URL-escape
	cursor2 := "cursor2"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		seenCursors = append(seenCursors, r.URL.Query().Get("cursor"))
		switch n {
		case 1:
			jsonWrite(t, w, http.StatusOK, map[string]any{
				"data": []map[string]any{
					{"checksum_sha256": hexsha(0x01), "format": "qcow2", "path": "/p/01", "size_bytes": 1},
					{"checksum_sha256": hexsha(0x02), "format": "qcow2", "path": "/p/02", "size_bytes": 2},
				},
				"meta": map[string]any{"next_cursor": cursor1},
			})
		case 2:
			jsonWrite(t, w, http.StatusOK, map[string]any{
				"data": []map[string]any{
					{"checksum_sha256": hexsha(0x03), "format": "qcow2", "path": "/p/03", "size_bytes": 3},
					{"checksum_sha256": hexsha(0x04), "format": "qcow2", "path": "/p/04", "size_bytes": 4},
				},
				"meta": map[string]any{"next_cursor": cursor2},
			})
		case 3:
			jsonWrite(t, w, http.StatusOK, map[string]any{
				"data": []map[string]any{
					{"checksum_sha256": hexsha(0x05), "format": "qcow2", "path": "/p/05", "size_bytes": 5},
				},
				"meta": map[string]any{"next_cursor": nil},
			})
		default:
			t.Errorf("unexpected fourth list request (cursor=%q)", r.URL.Query().Get("cursor"))
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	got, err := cli.ListImages(context.Background(), agentURL, poolID)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("len = %d, want 5 across 3 pages", len(got))
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
	if len(seenCursors) != 3 {
		t.Fatalf("seenCursors = %v, want 3 entries", seenCursors)
	}
	if seenCursors[0] != "" {
		t.Errorf("first cursor = %q, want empty", seenCursors[0])
	}
	// Server saw URL-decoded values; assert decoded form matches the
	// original (proves the client URL-escaped on the way out and the
	// stdlib server reversed it on the way in).
	if seenCursors[1] != cursor1 {
		t.Errorf("second cursor = %q, want %q", seenCursors[1], cursor1)
	}
	if seenCursors[2] != cursor2 {
		t.Errorf("third cursor = %q, want %q", seenCursors[2], cursor2)
	}
}

func TestListImages_EmptyPool(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)
	poolID := "test-pool"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/storage-pools/"+poolID+"/images", func(w http.ResponseWriter, _ *http.Request) {
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"data": []map[string]any{},
			"meta": map[string]any{"next_cursor": nil},
		})
	})
	_, agentURL := startAgentTLSServer(t, mux)

	cli := newClient(t, dir)

	got, err := cli.ListImages(context.Background(), agentURL, poolID)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 (empty pool)", len(got))
	}
}

// decodeReqJSON decodes r's JSON body into v and closes the body.
func decodeReqJSON(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(v)
}
