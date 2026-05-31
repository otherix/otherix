// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/api/response"
)

func TestHealthz(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	decodeJSON(t, resp, &body)
	if body.Status != "ok" {
		t.Errorf("/healthz status = %q, want ok", body.Status)
	}
}

func TestReadyzReportsEtcdCheck(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/readyz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	decodeJSON(t, resp, &body)
	if body.Status != "ok" {
		t.Errorf("/readyz status = %q, want ok", body.Status)
	}
	etcdCheck, ok := body.Checks["etcd"]
	if !ok {
		t.Fatalf("/readyz checks missing 'etcd'; got %#v", body.Checks)
	}
	if etcdCheck.Status != "ok" {
		t.Errorf("/readyz checks.etcd.status = %q, want ok", etcdCheck.Status)
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/v1/no-such-endpoint", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeNotFound {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeNotFound)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	h := newE2E(t)
	resp := h.get(t, "/v1/users", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
