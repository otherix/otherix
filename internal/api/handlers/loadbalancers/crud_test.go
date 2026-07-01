// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/loadbalancers"
	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// fakeStore is an in-memory double for loadbalancers.Store. It enforces the
// case-insensitive name-uniqueness guard and soft-delete semantics of the real
// etcd store, and records the last created row for owner-stamping assertions.
type fakeStore struct {
	byID        map[uuid.UUID]store.LoadBalancer
	byName      map[string]uuid.UUID // lower(name) -> id
	lastCreated store.LoadBalancer
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byID:   map[uuid.UUID]store.LoadBalancer{},
		byName: map[string]uuid.UUID{},
	}
}

func (f *fakeStore) CreateLoadBalancer(_ context.Context, arg store.CreateLoadBalancerParams) (store.LoadBalancer, error) {
	if _, ok := f.byName[strings.ToLower(arg.Name)]; ok {
		return store.LoadBalancer{}, store.ErrLoadBalancerNameExists
	}
	now := time.Now().UTC()
	lb := store.LoadBalancer{
		ID:        arg.ID,
		Name:      arg.Name,
		OwnerID:   arg.OwnerID,
		Port:      arg.Port,
		Selector:  arg.Selector,
		CreatedAt: now,
		UpdatedAt: now,
	}
	f.byID[lb.ID] = lb
	f.byName[strings.ToLower(lb.Name)] = lb.ID
	f.lastCreated = lb
	return lb, nil
}

func (f *fakeStore) LoadBalancerByName(_ context.Context, name string) (store.LoadBalancer, error) {
	id, ok := f.byName[strings.ToLower(name)]
	if !ok {
		return store.LoadBalancer{}, store.ErrNotFound
	}
	return f.byID[id], nil
}

func (f *fakeStore) UpdateLoadBalancer(_ context.Context, arg store.UpdateLoadBalancerParams) (store.LoadBalancer, error) {
	lb, ok := f.byID[arg.ID]
	if !ok {
		return store.LoadBalancer{}, store.ErrNotFound
	}
	if lower := strings.ToLower(arg.Name); lower != strings.ToLower(lb.Name) {
		if _, taken := f.byName[lower]; taken {
			return store.LoadBalancer{}, store.ErrLoadBalancerNameExists
		}
		delete(f.byName, strings.ToLower(lb.Name))
		f.byName[lower] = lb.ID
	}
	lb.Name = arg.Name
	lb.Port = arg.Port
	lb.Selector = arg.Selector
	lb.UpdatedAt = time.Now().UTC()
	f.byID[lb.ID] = lb
	return lb, nil
}

func (f *fakeStore) ListLoadBalancers(_ context.Context, _ store.ListLoadBalancersParams) ([]store.LoadBalancer, error) {
	out := make([]store.LoadBalancer, 0, len(f.byID))
	for _, lb := range f.byID {
		out = append(out, lb)
	}
	return out, nil
}

func (f *fakeStore) DeleteLoadBalancer(_ context.Context, id uuid.UUID) error {
	lb, ok := f.byID[id]
	if !ok {
		return store.ErrNotFound
	}
	delete(f.byName, strings.ToLower(lb.Name))
	delete(f.byID, id)
	return nil
}

// stubBroker satisfies loadbalancers.IngressBroker. CRUD never calls it.
type stubBroker struct{}

func (stubBroker) ResolveIngress(_ context.Context, _ store.VM, _ int) (vms.IngressResult, error) {
	return vms.IngressResult{}, nil
}

func newRouter(st loadbalancers.Store, user *auth.User) http.Handler {
	h := loadbalancers.New(st, stubBroker{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithUser(req.Context(), user)))
		})
	})
	r.Get("/v1/loadbalancers", h.List)
	r.Get("/v1/loadbalancers/{id}", h.Get)
	r.Post("/v1/loadbalancers", h.Create)
	r.Patch("/v1/loadbalancers/{id}", h.Update)
	r.Delete("/v1/loadbalancers/{id}", h.Delete)
	return r
}

func do(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	return env.Error.Code
}

func devUser() *auth.User {
	return &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper, Type: auth.TypeJWT}
}

func TestCreateLoadBalancerStampsOwner(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	body := `{"name":"web","port":8080,"selector":{"app":"web"}}`
	rec := do(t, router, http.MethodPost, "/v1/loadbalancers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := st.lastCreated.OwnerID; got != u.ID {
		t.Errorf("persisted OwnerID = %v, want caller %v", got, u.ID)
	}

	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if view["owner_id"] != u.ID.String() {
		t.Errorf("response owner_id = %v, want %v", view["owner_id"], u.ID)
	}
	if view["name"] != "web" {
		t.Errorf("response name = %v, want web", view["name"])
	}
}

func TestGetLoadBalancerByName(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())

	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["name"] != "web" {
		t.Errorf("name = %v, want web", view["name"])
	}
}

func TestGetLoadBalancerNotFound(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListLoadBalancers(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	for _, n := range []string{"a", "b", "c"} {
		if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
			`{"name":"`+n+`","port":80,"selector":{"app":"`+n+`"}}`); rec.Code != http.StatusCreated {
			t.Fatalf("create %s status = %d; body=%s", n, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, router, http.MethodGet, "/v1/loadbalancers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Errorf("list len = %d, want 3", len(resp.Data))
	}
}

func TestUpdateLoadBalancerByName(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, router, http.MethodPatch, "/v1/loadbalancers/web", `{"port":9090}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["port"] != float64(9090) {
		t.Errorf("port = %v, want 9090", view["port"])
	}
}

func TestUpdateCrossOwnerReturns404(t *testing.T) {
	st := newFakeStore()
	owner := devUser()
	other := devUser()

	// Owner creates the load balancer.
	if rec := do(t, newRouter(st, owner), http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// A different developer must not see it: cross-owner update -> 404, not 403.
	rec := do(t, newRouter(st, other), http.MethodPatch, "/v1/loadbalancers/web", `{"port":9090}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner update status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteLoadBalancerByName(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, router, http.MethodDelete, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateSelectorValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty map", `{"name":"web","port":80,"selector":{}}`},
		{"missing selector", `{"name":"web","port":80}`},
		{"empty key", `{"name":"web","port":80,"selector":{"":"v"}}`},
		{"empty value", `{"name":"web","port":80,"selector":{"app":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := newRouter(newFakeStore(), devUser())
			rec := do(t, router, http.MethodPost, "/v1/loadbalancers", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
		})
	}
}

func TestCreatePortValidation(t *testing.T) {
	for _, body := range []string{
		`{"name":"web","port":0,"selector":{"app":"web"}}`,
		`{"name":"web","port":70000,"selector":{"app":"web"}}`,
		`{"name":"","port":80,"selector":{"app":"web"}}`,
	} {
		router := newRouter(newFakeStore(), devUser())
		rec := do(t, router, http.MethodPost, "/v1/loadbalancers", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400; resp=%s", body, rec.Code, rec.Body.String())
		}
	}
}
