// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// deleterSpy records the digests Delete was driven with. It stands in for the
// production artifact store (whose Delete removes a real blob plus its checksum
// sidecar).
type deleterSpy struct {
	digests []string
}

func (s *deleterSpy) Delete(digest string) error {
	s.digests = append(s.digests, digest)
	return nil
}

func TestReclaim_HappyPath(t *testing.T) {
	digest := strings.Repeat("a", 64)
	del := &deleterSpy{}
	h := New(&serveSpy{}, &pullSpy{}, &stopSpy{}, del, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{"digest": digest})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/reclaim", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if len(del.digests) != 1 || del.digests[0] != digest {
		t.Errorf("Delete digests = %v, want [%s]", del.digests, digest)
	}
}

func TestReclaim_BadRequestOnNonHexDigest(t *testing.T) {
	del := &deleterSpy{}
	h := New(&serveSpy{}, &pullSpy{}, &stopSpy{}, del, discardLogger())
	router := mountTestRouter(h)

	body, _ := json.Marshal(map[string]any{"digest": "not-a-valid-digest"})
	req := httptest.NewRequest(http.MethodPost, "/v1/blobs/reclaim", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if len(del.digests) != 0 {
		t.Errorf("Delete digests = %v, want none (handler must reject before dispatch)", del.digests)
	}
}
