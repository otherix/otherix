// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/users"
	"github.com/otherix/otherix/internal/auth"
)

// newListHarness wires the users handler behind a stub admin authn layer for
// GET /v1/users.
func newListHarness(fake users.Store) http.Handler {
	h := users.New(fake)
	admin := &auth.User{ID: uuid.New(), Role: auth.RoleAdmin, Type: auth.TypeJWT}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUser(req.Context(), admin)))
		})
	})
	r.Get("/v1/users", h.List)
	return r
}

func getReq(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestListUsersFilterByUsername(t *testing.T) {
	fake := newCreateFakeStore()
	createHarness := newCreateHarness(fake)
	for _, name := range []string{"alice", "bob"} {
		body := `{"username":"` + name + `","password":"a-valid-password-123","role":"developer"}`
		if rec := postJSON(t, createHarness, "/v1/users", body); rec.Code != http.StatusCreated {
			t.Fatalf("seed %s status = %d (body %s)", name, rec.Code, rec.Body.String())
		}
	}

	listHarness := newListHarness(fake)

	// Hit: exactly one matching row.
	rec := getReq(t, listHarness, "/v1/users?username=alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("?username=alice status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	data := listData(t, rec)
	if len(data) != 1 {
		t.Fatalf("?username=alice returned %d rows, want 1", len(data))
	}
	if got := data[0]["username"]; got != "alice" {
		t.Errorf("?username=alice row username = %v, want alice", got)
	}

	// Miss: empty page, not 404.
	rec = getReq(t, listHarness, "/v1/users?username=nobody")
	if rec.Code != http.StatusOK {
		t.Fatalf("?username=nobody status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if data := listData(t, rec); len(data) != 0 {
		t.Errorf("?username=nobody returned %d rows, want 0", len(data))
	}
}

func listData(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list response: %v (body %s)", err, rec.Body.String())
	}
	return body.Data
}
