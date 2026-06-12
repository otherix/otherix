// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// createStoreStub satisfies the handler's Store interface for the Create
// tests. It embeds Store (nil) so only CreateNode has a body; any other
// call panics - the desired tripwire, since validation failures must
// short-circuit before any store work.
type createStoreStub struct {
	Store
	created *store.CreateNodeParams
}

func (s *createStoreStub) CreateNode(_ context.Context, arg store.CreateNodeParams) (store.Node, error) {
	s.created = &arg
	return store.Node{
		ID:                      arg.ID,
		Name:                    arg.Name,
		Architecture:            arg.Architecture,
		AdvertisedEndpoint:      arg.AdvertisedEndpoint,
		MigrationHost:           arg.MigrationHost,
		MigrationPortRangeStart: arg.MigrationPortRangeStart,
		MigrationPortRangeEnd:   arg.MigrationPortRangeEnd,
		Status:                  arg.Status,
	}, nil
}

// newCreateRequest builds a POST /v1/nodes request with the given name
// and otherwise-valid required fields.
func newCreateRequest(t *testing.T, name string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":                       name,
		"architecture":               "amd64",
		"advertised_endpoint":        "https://agent.example.test:8443",
		"migration_host":             "10.0.0.1",
		"migration_port_range_start": 49152,
		"migration_port_range_end":   49251,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, "/v1/nodes", bytes.NewReader(body))
}

// TestCreateRejectsNonDNSLabelName pins the wire contract for the node
// name rule (audit LOW): the name flows into the issued cert CN
// `node-<name>` and SAN, so a non-DNS-label name must yield 400 with
// the validation_failed code before any store work.
func TestCreateRejectsNonDNSLabelName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Bad_Name", "a/b", "node.local", "-node", "node-"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := New(&createStoreStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			rec := httptest.NewRecorder()
			h.Create(rec, newCreateRequest(t, name))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("Create(name=%q) status = %d, want %d", name, rec.Code, http.StatusBadRequest)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error.Code != "validation_failed" {
				t.Errorf("error.code = %q, want %q", body.Error.Code, "validation_failed")
			}
		})
	}
}

// TestCreateAcceptsDNSLabelName confirms a valid lowercase DNS label
// still passes the name check and reaches the store.
func TestCreateAcceptsDNSLabelName(t *testing.T) {
	t.Parallel()

	stub := &createStoreStub{}
	h := New(stub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.Create(rec, newCreateRequest(t, "node-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("Create(name=%q) status = %d, want %d; body: %s",
			"node-1", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if stub.created == nil {
		t.Fatal("CreateNode was not called")
	}
	if stub.created.Name != "node-1" {
		t.Errorf("persisted name = %q, want %q", stub.created.Name, "node-1")
	}
}
