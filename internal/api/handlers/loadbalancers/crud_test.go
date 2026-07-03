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
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// fakeStore is an in-memory double for loadbalancers.Store. It enforces the
// case-insensitive name-uniqueness guard and soft-delete semantics of the real
// etcd store, and records the last created row for owner-stamping assertions.
type fakeStore struct {
	byID            map[uuid.UUID]store.LoadBalancer
	byName          map[string]uuid.UUID // lower(name) -> id
	byPublishedPort map[int32]uuid.UUID  // published port -> id (uniqueness guard)
	revByID         map[uuid.UUID]int64  // id -> synthetic ModRevision (bumps per write)
	updateConflicts map[uuid.UUID]int    // id -> remaining forced ErrLoadBalancerConflict on update
	lastCreated     store.LoadBalancer

	vms        map[uuid.UUID]store.VM
	runtimes   map[uuid.UUID]store.VMRuntime
	runtimeErr map[uuid.UUID]error // injected transient VMRuntimeByID error

	// lbHealth holds observed backend-health verdicts keyed by (lb id, vm id);
	// healthErr injects a transient ListLBBackendHealth failure per lb id.
	lbHealth  map[uuid.UUID]map[uuid.UUID]store.LBBackendHealth
	healthErr map[uuid.UUID]error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byID:            map[uuid.UUID]store.LoadBalancer{},
		byName:          map[string]uuid.UUID{},
		byPublishedPort: map[int32]uuid.UUID{},
		revByID:         map[uuid.UUID]int64{},
		updateConflicts: map[uuid.UUID]int{},
		vms:             map[uuid.UUID]store.VM{},
		runtimes:        map[uuid.UUID]store.VMRuntime{},
		runtimeErr:      map[uuid.UUID]error{},
		lbHealth:        map[uuid.UUID]map[uuid.UUID]store.LBBackendHealth{},
		healthErr:       map[uuid.UUID]error{},
	}
}

// ListLBBackendHealth returns a copy of the seeded verdicts for the load
// balancer, or an injected transient error.
func (f *fakeStore) ListLBBackendHealth(_ context.Context, lbID uuid.UUID) (map[uuid.UUID]store.LBBackendHealth, error) {
	if err := f.healthErr[lbID]; err != nil {
		return nil, err
	}
	src := f.lbHealth[lbID]
	out := make(map[uuid.UUID]store.LBBackendHealth, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

// seedHealth records an observed active-health verdict for one (lb, vm) pair.
func (f *fakeStore) seedHealth(lbID, vmID uuid.UUID, healthy bool, reportedAt time.Time) {
	if f.lbHealth[lbID] == nil {
		f.lbHealth[lbID] = map[uuid.UUID]store.LBBackendHealth{}
	}
	f.lbHealth[lbID][vmID] = store.LBBackendHealth{Healthy: healthy, ReportedAt: reportedAt}
}

// ListVMsByOwner returns the owner's non-deleted VMs (order is unspecified,
// mirroring the etcd range).
func (f *fakeStore) ListVMsByOwner(_ context.Context, owner uuid.UUID) ([]store.VM, error) {
	out := make([]store.VM, 0, len(f.vms))
	for _, vm := range f.vms {
		if vm.OwnerID == owner && vm.DeletedAt == nil {
			out = append(out, vm)
		}
	}
	return out, nil
}

// VMRuntimeByID returns the seeded runtime, an injected error, or ErrNotFound.
func (f *fakeStore) VMRuntimeByID(_ context.Context, id uuid.UUID) (store.VMRuntime, error) {
	if err := f.runtimeErr[id]; err != nil {
		return store.VMRuntime{}, err
	}
	rt, ok := f.runtimes[id]
	if !ok {
		return store.VMRuntime{}, store.ErrNotFound
	}
	return rt, nil
}

// seedLB inserts a load balancer row directly (bypassing the create handler).
func (f *fakeStore) seedLB(t *testing.T, name string, owner uuid.UUID, port int32, selector map[string]string) store.LoadBalancer {
	t.Helper()
	now := time.Now().UTC()
	lb := store.LoadBalancer{
		ID:        uuid.New(),
		Name:      name,
		OwnerID:   owner,
		Port:      port,
		Selector:  selector,
		CreatedAt: now,
		UpdatedAt: now,
	}
	f.byID[lb.ID] = lb
	f.byName[strings.ToLower(name)] = lb.ID
	f.revByID[lb.ID] = 1
	return lb
}

func (f *fakeStore) seedVM(owner uuid.UUID, labels string) store.VM {
	vm := store.VM{ID: uuid.New(), OwnerID: owner, Name: "vm-" + uuid.NewString()[:8], Labels: []byte(labels)}
	f.vms[vm.ID] = vm
	return vm
}

// seedRunningVM seeds a VM with a runtime observed in the running phase.
func (f *fakeStore) seedRunningVM(_ *testing.T, owner uuid.UUID, labels string) store.VM {
	vm := f.seedVM(owner, labels)
	f.runtimes[vm.ID] = store.VMRuntime{VmID: vm.ID, Phase: store.VmPhaseRunning}
	return vm
}

// seedStoppedVM seeds a VM whose runtime is observed stopped (never eligible).
func (f *fakeStore) seedStoppedVM(_ *testing.T, owner uuid.UUID, labels string) store.VM {
	vm := f.seedVM(owner, labels)
	f.runtimes[vm.ID] = store.VMRuntime{VmID: vm.ID, Phase: store.VmPhaseStopped}
	return vm
}

func (f *fakeStore) CreateLoadBalancer(_ context.Context, arg store.CreateLoadBalancerParams) (store.LoadBalancer, error) {
	if _, ok := f.byName[strings.ToLower(arg.Name)]; ok {
		return store.LoadBalancer{}, store.ErrLoadBalancerNameExists
	}
	if arg.PublishedPort != nil {
		if _, taken := f.byPublishedPort[*arg.PublishedPort]; taken {
			return store.LoadBalancer{}, store.ErrLoadBalancerPublishedPortExists
		}
	}
	now := time.Now().UTC()
	lb := store.LoadBalancer{
		ID:            arg.ID,
		Name:          arg.Name,
		OwnerID:       arg.OwnerID,
		Port:          arg.Port,
		Selector:      arg.Selector,
		HealthCheck:   arg.HealthCheck,
		PublishedPort: arg.PublishedPort,
		Protocol:      arg.Protocol,
		SourceCIDRs:   arg.SourceCIDRs,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	f.byID[lb.ID] = lb
	f.byName[strings.ToLower(lb.Name)] = lb.ID
	if lb.PublishedPort != nil {
		f.byPublishedPort[*lb.PublishedPort] = lb.ID
	}
	f.revByID[lb.ID] = 1
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

// LoadBalancerByNameWithRevision mirrors the real store: it returns the row and
// its synthetic ModRevision so the update handler can gate the write on it.
func (f *fakeStore) LoadBalancerByNameWithRevision(_ context.Context, name string) (store.LoadBalancer, int64, error) {
	id, ok := f.byName[strings.ToLower(name)]
	if !ok {
		return store.LoadBalancer{}, 0, store.ErrNotFound
	}
	return f.byID[id], f.revByID[id], nil
}

func (f *fakeStore) UpdateLoadBalancer(_ context.Context, arg store.UpdateLoadBalancerParams) (store.LoadBalancer, error) {
	lb, ok := f.byID[arg.ID]
	if !ok {
		return store.LoadBalancer{}, store.ErrNotFound
	}
	// Injected transient conflict: model a concurrent writer that keeps winning
	// the CAS for the first N attempts, so the handler's retry loop is exercised.
	if n := f.updateConflicts[arg.ID]; n > 0 {
		f.updateConflicts[arg.ID] = n - 1
		return store.LoadBalancer{}, store.ErrLoadBalancerConflict
	}
	// Optimistic-concurrency gate, mirroring the etcd ModRevision CAS.
	if arg.ExpectedRevision > 0 && arg.ExpectedRevision != f.revByID[arg.ID] {
		return store.LoadBalancer{}, store.ErrLoadBalancerConflict
	}
	if lower := strings.ToLower(arg.Name); lower != strings.ToLower(lb.Name) {
		if _, taken := f.byName[lower]; taken {
			return store.LoadBalancer{}, store.ErrLoadBalancerNameExists
		}
		delete(f.byName, strings.ToLower(lb.Name))
		f.byName[lower] = lb.ID
	}
	if arg.PublishedPort != nil {
		if owner, taken := f.byPublishedPort[*arg.PublishedPort]; taken && owner != lb.ID {
			return store.LoadBalancer{}, store.ErrLoadBalancerPublishedPortExists
		}
	}
	if lb.PublishedPort != nil {
		delete(f.byPublishedPort, *lb.PublishedPort)
	}
	lb.Name = arg.Name
	lb.Port = arg.Port
	lb.Selector = arg.Selector
	lb.HealthCheck = arg.HealthCheck
	lb.PublishedPort = arg.PublishedPort
	lb.Protocol = arg.Protocol
	lb.SourceCIDRs = arg.SourceCIDRs
	lb.UpdatedAt = time.Now().UTC()
	f.byID[lb.ID] = lb
	f.revByID[lb.ID]++
	if lb.PublishedPort != nil {
		f.byPublishedPort[*lb.PublishedPort] = lb.ID
	}
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

func opUser() *auth.User {
	return &auth.User{ID: uuid.New(), Role: auth.RoleOperator, Type: auth.TypeJWT}
}

// TestCreatePublishedRequiresPublishPermission asserts a caller without
// loadbalancer:publish (developer) cannot create a published LB (403), while an
// operator can (201).
func TestCreatePublishedRequiresPublishPermission(t *testing.T) {
	st := newFakeStore()
	body := `{"name":"web","port":80,"selector":{"app":"web"},"published_port":8080}`

	devRec := do(t, newRouter(st, devUser()), http.MethodPost, "/v1/loadbalancers", body)
	if devRec.Code != http.StatusForbidden {
		t.Fatalf("developer publish create = %d, want 403; body=%s", devRec.Code, devRec.Body.String())
	}
	if code := errorCode(t, devRec); code != "permission_denied" {
		t.Errorf("code = %q, want permission_denied", code)
	}

	opRec := do(t, newRouter(st, opUser()), http.MethodPost, "/v1/loadbalancers", body)
	if opRec.Code != http.StatusCreated {
		t.Fatalf("operator publish create = %d, want 201; body=%s", opRec.Code, opRec.Body.String())
	}
}

// TestCreatePublishedDuplicatePort asserts a second LB claiming an
// already-published port is rejected with 409 conflict.
func TestCreatePublishedDuplicatePort(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, opUser())

	first := `{"name":"a","port":80,"selector":{"app":"a"},"published_port":8080}`
	if r := do(t, router, http.MethodPost, "/v1/loadbalancers", first); r.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201; body=%s", r.Code, r.Body.String())
	}
	second := `{"name":"b","port":80,"selector":{"app":"b"},"published_port":8080}`
	r := do(t, router, http.MethodPost, "/v1/loadbalancers", second)
	if r.Code != http.StatusConflict {
		t.Fatalf("dup published_port = %d, want 409; body=%s", r.Code, r.Body.String())
	}
	if code := errorCode(t, r); code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}
}

// TestCreatePublishedInvalidSourceCIDR asserts a malformed source_cidrs entry is
// rejected with 400.
func TestCreatePublishedInvalidSourceCIDR(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, opUser())
	body := `{"name":"web","port":80,"selector":{"app":"web"},"published_port":8080,"source_cidrs":["not-a-cidr"]}`
	r := do(t, router, http.MethodPost, "/v1/loadbalancers", body)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("invalid source cidr = %d, want 400; body=%s", r.Code, r.Body.String())
	}
	if code := errorCode(t, r); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

// TestCreatePublishedTooManySourceCIDRs asserts a source_cidrs list longer than
// MaxSourceCIDRs is rejected with 400 rather than persisting an unbounded
// allowlist.
func TestCreatePublishedTooManySourceCIDRs(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, opUser())
	cidrs := strings.TrimSuffix(strings.Repeat(`"10.0.0.0/8",`, validation.MaxSourceCIDRs+1), ",")
	body := `{"name":"web","port":80,"selector":{"app":"web"},"published_port":8080,"source_cidrs":[` + cidrs + `]}`
	r := do(t, router, http.MethodPost, "/v1/loadbalancers", body)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("too many source cidrs = %d, want 400; body=%s", r.Code, r.Body.String())
	}
	if code := errorCode(t, r); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

// TestCreatePublishFieldsRequirePublishedPort asserts that a create carrying
// publish-only fields (protocol, source_cidrs) without a published_port is
// rejected with 400. Those fields have no meaning on an unpublished LB, and
// accepting them would persist an unvalidated allowlist that a later publish
// would silently carry into the exposed listener.
func TestCreatePublishFieldsRequirePublishedPort(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"source_cidrs without published_port", `{"name":"web","port":80,"selector":{"app":"web"},"source_cidrs":["garbage"]}`},
		{"protocol without published_port", `{"name":"web","port":80,"selector":{"app":"web"},"protocol":"tcp"}`},
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

// TestUpdatePublishFieldsRequirePublishPermission asserts the update publish
// gate covers the whole exposure surface: an owner with only loadbalancer:update
// (developer) cannot strip the source-CIDR allowlist on a published LB, even
// though source_cidrs is not published_port.
func TestUpdatePublishFieldsRequirePublishPermission(t *testing.T) {
	st := newFakeStore()
	owner := devUser()
	// A published LB owned by the developer (as if an operator published it).
	port := int32(8080)
	now := time.Now().UTC()
	lb := store.LoadBalancer{
		ID:            uuid.New(),
		Name:          "web",
		OwnerID:       owner.ID,
		Port:          80,
		Selector:      map[string]string{"app": "web"},
		PublishedPort: &port,
		Protocol:      "tcp",
		SourceCIDRs:   []string{"10.0.0.0/8"},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	st.byID[lb.ID] = lb
	st.byName[strings.ToLower(lb.Name)] = lb.ID
	st.byPublishedPort[port] = lb.ID

	rec := do(t, newRouter(st, owner), http.MethodPatch, "/v1/loadbalancers/web", `{"source_cidrs":[]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("developer stripping allowlist = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "permission_denied" {
		t.Errorf("code = %q, want permission_denied", code)
	}
}

// TestUpdatePublishOnlyFieldsRejectedWhenUnpublished asserts that an update
// carrying publish-only fields (protocol, source_cidrs) that leaves the row
// unpublished is rejected with 400, matching the create path, so no inert
// exposure state is persisted. The unpublish sentinel (published_port:0), which
// clears all three fields together, still succeeds.
func TestUpdatePublishOnlyFieldsRejectedWhenUnpublished(t *testing.T) {
	op := opUser()
	now := time.Now().UTC()

	t.Run("source_cidrs on unpublished LB", func(t *testing.T) {
		st := newFakeStore()
		lb := store.LoadBalancer{
			ID:        uuid.New(),
			Name:      "u1",
			OwnerID:   op.ID,
			Port:      80,
			Selector:  map[string]string{"app": "u"},
			CreatedAt: now,
			UpdatedAt: now,
		}
		st.byID[lb.ID] = lb
		st.byName[strings.ToLower(lb.Name)] = lb.ID

		rec := do(t, newRouter(st, op), http.MethodPatch, "/v1/loadbalancers/u1", `{"source_cidrs":["10.0.0.0/8"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unpublished source_cidrs = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "validation_failed" {
			t.Errorf("code = %q, want validation_failed", code)
		}
	})

	t.Run("unpublish sentinel clears all three", func(t *testing.T) {
		st := newFakeStore()
		port := int32(8080)
		lb := store.LoadBalancer{
			ID:            uuid.New(),
			Name:          "p1",
			OwnerID:       op.ID,
			Port:          80,
			Selector:      map[string]string{"app": "p"},
			PublishedPort: &port,
			Protocol:      "tcp",
			SourceCIDRs:   []string{"10.0.0.0/8"},
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		st.byID[lb.ID] = lb
		st.byName[strings.ToLower(lb.Name)] = lb.ID
		st.byPublishedPort[port] = lb.ID

		rec := do(t, newRouter(st, op), http.MethodPatch, "/v1/loadbalancers/p1", `{"published_port":0}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("unpublish = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
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

func TestGetLoadBalancerBackends(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	lb := st.seedLB(t, "web", u.ID, 8080, map[string]string{"app": "web"})
	healthy := st.seedVM(u.ID, `{"app":"web"}`)
	st.seedVM(u.ID, `{"app":"web"}`) // warming: no health record -> healthy null
	staleVM := st.seedVM(u.ID, `{"app":"web"}`)
	reported := time.Now().UTC().Truncate(time.Second)
	st.seedHealth(lb.ID, healthy.ID, true, reported)
	// A record older than the (heartbeat-floored, 90s) freshness window renders
	// as absent (healthy null), matching connect eligibility and the spec.
	st.seedHealth(lb.ID, staleVM.ID, false, reported.Add(-200*time.Second))

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Backends []struct {
			VMID       string  `json:"vm_id"`
			VMName     string  `json:"vm_name"`
			Healthy    *bool   `json:"healthy"`
			ReportedAt *string `json:"reported_at"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Backends) != 3 {
		t.Fatalf("backends len = %d, want 3; body=%s", len(view.Backends), rec.Body.String())
	}
	for i := 1; i < len(view.Backends); i++ {
		if a, b := view.Backends[i-1].VMName, view.Backends[i].VMName; a > b {
			t.Errorf("backends not sorted by vm_name: %q then %q", a, b)
		}
	}

	for _, b := range view.Backends {
		switch b.VMID {
		case healthy.ID.String():
			if b.Healthy == nil || !*b.Healthy {
				t.Errorf("recorded backend healthy = %v, want true", b.Healthy)
			}
			if b.ReportedAt == nil {
				t.Errorf("recorded backend reported_at = nil, want non-null")
			}
		case staleVM.ID.String(): // stale record -> treated as absent -> null
			if b.Healthy != nil {
				t.Errorf("stale backend healthy = %v, want null (stale record rendered as absent)", *b.Healthy)
			}
			if b.ReportedAt != nil {
				t.Errorf("stale backend reported_at = %v, want null", *b.ReportedAt)
			}
		default: // warming backend: no health record
			if b.Healthy != nil {
				t.Errorf("warming backend healthy = %v, want null", *b.Healthy)
			}
			if b.ReportedAt != nil {
				t.Errorf("warming backend reported_at = %v, want null", *b.ReportedAt)
			}
		}
	}
}

// healthSummaryJSON mirrors the optional health summary the get and list
// projections attach.
type healthSummaryJSON struct {
	Status         string `json:"status"`
	TargetsTotal   int    `json:"targets_total"`
	TargetsHealthy int    `json:"targets_healthy"`
}

// TestGetLoadBalancerHealthSummary asserts the single-resource get attaches the
// aggregate health summary alongside the enumerated backends. Two selector-
// matched backends: one fresh-healthy, one warming (no record) -> total 2,
// healthy 1, status degraded.
func TestGetLoadBalancerHealthSummary(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	lb := seedLBWithInterval(t, st, "web", u.ID, 8080, map[string]string{"app": "web"})
	healthy := st.seedVM(u.ID, `{"app":"web"}`)
	st.seedVM(u.ID, `{"app":"web"}`) // warming: no health record
	st.seedHealth(lb.ID, healthy.ID, true, time.Now())

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Health *healthSummaryJSON `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Health == nil {
		t.Fatalf("get response omitted health summary; body=%s", rec.Body.String())
	}
	want := healthSummaryJSON{Status: "degraded", TargetsTotal: 2, TargetsHealthy: 1}
	if *view.Health != want {
		t.Errorf("health = %+v, want %+v", *view.Health, want)
	}
}

// TestListLoadBalancersHealthSummary drives the real HTTP list: an LB with two
// selector-matched backends, one fresh-healthy and one fresh-unhealthy, reports
// health {status:degraded, targets_total:2, targets_healthy:1}. The list
// projection carries only the scalar summary, never the enumerated backends.
func TestListLoadBalancersHealthSummary(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	lb := seedLBWithInterval(t, st, "web", u.ID, 8080, map[string]string{"app": "web"})
	vmA := st.seedVM(u.ID, `{"app":"web"}`)
	vmB := st.seedVM(u.ID, `{"app":"web"}`)
	now := time.Now()
	st.seedHealth(lb.ID, vmA.ID, true, now)
	st.seedHealth(lb.ID, vmB.ID, false, now)

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			Health   *healthSummaryJSON `json:"health"`
			Backends []json.RawMessage  `json:"backends"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("list len = %d, want 1; body=%s", len(resp.Data), rec.Body.String())
	}
	if resp.Data[0].Health == nil {
		t.Fatalf("list omitted health summary; body=%s", rec.Body.String())
	}
	want := healthSummaryJSON{Status: "degraded", TargetsTotal: 2, TargetsHealthy: 1}
	if *resp.Data[0].Health != want {
		t.Errorf("health = %+v, want %+v", *resp.Data[0].Health, want)
	}
	if len(resp.Data[0].Backends) != 0 {
		t.Errorf("list projection enumerated %d backends, want 0 (only the scalar summary)", len(resp.Data[0].Backends))
	}
}

// TestCreateResponseOmitsHealth asserts the create response has no live-health
// context and therefore omits the optional health key entirely (toView leaves
// it nil).
func TestCreateResponseOmitsHealth(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := view["health"]; ok {
		t.Errorf("create response carried a health key; want it omitted: %v", view["health"])
	}
}

func TestGetLoadBalancerNotFound(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "loadbalancer_not_found" {
		t.Errorf("code = %q, want loadbalancer_not_found", code)
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

// TestUpdateRetriesTransientConflict proves the handler re-reads and re-applies
// on a concurrency conflict: two forced conflicts are absorbed by the bounded
// retry loop and the third attempt commits, so the client still sees 200.
func TestUpdateRetriesTransientConflict(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}
	st.updateConflicts[st.lastCreated.ID] = 2 // absorbed by the 4-attempt loop

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

// TestUpdatePersistentConflictReturns409 proves the retry loop is bounded: a
// writer that keeps winning past the attempt budget surfaces as a 409 rather
// than looping forever.
func TestUpdatePersistentConflictReturns409(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}
	st.updateConflicts[st.lastCreated.ID] = 100 // never resolves within the budget

	rec := do(t, router, http.MethodPatch, "/v1/loadbalancers/web", `{"port":9090}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("update status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "conflict" {
		t.Errorf("error code = %q, want %q", code, "conflict")
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

func TestDeleteCrossOwnerReturns404(t *testing.T) {
	st := newFakeStore()
	owner := devUser()
	other := devUser()

	if rec := do(t, newRouter(st, owner), http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// A different developer must not learn it exists: cross-owner delete -> 404.
	rec := do(t, newRouter(st, other), http.MethodDelete, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner delete status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "loadbalancer_not_found" {
		t.Errorf("code = %q, want loadbalancer_not_found", code)
	}
}

func TestUpdateSelectorValidation(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// An empty selector map is invalid on the update path too.
	rec := do(t, router, http.MethodPatch, "/v1/loadbalancers/web", `{"selector":{}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update selector status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
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

func TestCreateHealthCheckDefaultsFollowTrafficPort(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())

	rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// Stored config keeps the follow sentinel (Port==0).
	if got := st.lastCreated.HealthCheck.Port; got != 0 {
		t.Errorf("stored HealthCheck.Port = %d, want 0 (follow sentinel)", got)
	}

	var view struct {
		HealthCheck struct {
			Port               int32 `json:"port"`
			IntervalSeconds    int32 `json:"interval_seconds"`
			TimeoutSeconds     int32 `json:"timeout_seconds"`
			HealthyThreshold   int32 `json:"healthy_threshold"`
			UnhealthyThreshold int32 `json:"unhealthy_threshold"`
		} `json:"health_check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.HealthCheck.Port != 8080 {
		t.Errorf("view health_check.port = %d, want 8080 (follows traffic port)", view.HealthCheck.Port)
	}
	if view.HealthCheck.IntervalSeconds != store.HealthCheckDefaultIntervalSeconds {
		t.Errorf("view interval_seconds = %d, want %d", view.HealthCheck.IntervalSeconds, store.HealthCheckDefaultIntervalSeconds)
	}
	if view.HealthCheck.TimeoutSeconds != store.HealthCheckDefaultTimeoutSeconds {
		t.Errorf("view timeout_seconds = %d, want %d", view.HealthCheck.TimeoutSeconds, store.HealthCheckDefaultTimeoutSeconds)
	}
	if view.HealthCheck.HealthyThreshold != store.HealthCheckDefaultHealthyThreshold {
		t.Errorf("view healthy_threshold = %d, want %d", view.HealthCheck.HealthyThreshold, store.HealthCheckDefaultHealthyThreshold)
	}
	if view.HealthCheck.UnhealthyThreshold != store.HealthCheckDefaultUnhealthyThreshold {
		t.Errorf("view unhealthy_threshold = %d, want %d", view.HealthCheck.UnhealthyThreshold, store.HealthCheckDefaultUnhealthyThreshold)
	}
}

func TestCreateHealthCheckValidation(t *testing.T) {
	router := newRouter(newFakeStore(), devUser())
	body := `{"name":"web","port":8080,"selector":{"app":"web"},"health_check":{"interval_seconds":0}}`
	rec := do(t, router, http.MethodPost, "/v1/loadbalancers", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

func TestUpdateHealthCheckPortPins(t *testing.T) {
	st := newFakeStore()
	router := newRouter(st, devUser())
	if rec := do(t, router, http.MethodPost, "/v1/loadbalancers",
		`{"name":"web","port":8080,"selector":{"app":"web"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := do(t, router, http.MethodPatch, "/v1/loadbalancers/web", `{"health_check":{"port":9090}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		HealthCheck struct {
			Port int32 `json:"port"`
		} `json:"health_check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.HealthCheck.Port != 9090 {
		t.Errorf("view health_check.port = %d, want 9090", view.HealthCheck.Port)
	}
}

// TestUpdatePortFollowMovesHealthPort creates an LB with no health_check block
// (stored HealthCheck.Port == 0, the follow sentinel), then moves the traffic
// port with a PATCH that carries no health_check block. The effective health
// port must track the new traffic port.
func TestUpdatePortFollowMovesHealthPort(t *testing.T) {
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
	var view struct {
		HealthCheck struct {
			Port int32 `json:"port"`
		} `json:"health_check"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.HealthCheck.Port != 9090 {
		t.Errorf("view health_check.port = %d, want 9090 (follows new traffic port)", view.HealthCheck.Port)
	}
}

// TestUpdatePreFeatureRowNameOnly seeds a row with a zero HealthCheck (a row
// created before the health-check feature) and PATCHes only the name with no
// health_check block. The update must succeed (200), not fail 400 on the
// pre-feature zero cadence.
func TestUpdatePreFeatureRowNameOnly(t *testing.T) {
	st := newFakeStore()
	user := devUser()
	// seedLB inserts a row with the zero-value HealthCheck, matching a
	// pre-feature row exactly.
	st.seedLB(t, "web", user.ID, 8080, map[string]string{"app": "web"})
	router := newRouter(st, user)

	rec := do(t, router, http.MethodPatch, "/v1/loadbalancers/web", `{"name":"web2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", rec.Code, rec.Body.String())
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
