// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/loadbalancers"
	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// recordingBroker records the last VM it was asked to resolve and returns a
// gateway result addressing that VM, so a connect test can observe which backend
// the handler picked.
type recordingBroker struct {
	mu     sync.Mutex
	lastVM uuid.UUID
}

func (b *recordingBroker) ResolveIngress(_ context.Context, vm store.VM, port int) (vms.IngressResult, error) {
	b.mu.Lock()
	b.lastVM = vm.ID
	b.mu.Unlock()
	return vms.IngressResult{
		Transport:         "gateway",
		VMID:              vm.ID,
		VMName:            vm.Name,
		Port:              port,
		SplicerAddr:       "https://gw.test:9444",
		SplicerServerName: "node-gw.agents.otherix.local",
		SessionCred:       "cred",
	}, nil
}

func newConnectTestHandler(t *testing.T) (*loadbalancers.Handler, *fakeStore, *recordingBroker) {
	t.Helper()
	st := newFakeStore()
	broker := &recordingBroker{}
	h := loadbalancers.New(st, broker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h, st, broker
}

// doAuthedRequest drives the connect handler through a chi mux so chi.URLParam
// resolves {id}, with the given user injected into the request context.
func doAuthedRequest(t *testing.T, handler http.HandlerFunc, u *auth.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.MethodFunc(method, "/v1/loadbalancers/{id}/connect", handler)

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req = req.WithContext(auth.WithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestConnectBalancesOverEligibleBackends(t *testing.T) {
	h, st, broker := newConnectTestHandler(t)
	owner := uuid.New()
	lb := st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	a := st.seedRunningVM(t, owner, `{"app":"web"}`)
	b := st.seedRunningVM(t, owner, `{"app":"web"}`)
	st.seedStoppedVM(t, owner, `{"app":"web"}`) // matching but not running
	st.seedRunningVM(t, owner, `{"app":"db"}`)  // running but not matching
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	seen := map[uuid.UUID]int{}
	for i := 0; i < 40; i++ {
		rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("connect status = %d, body=%s", rec.Code, rec.Body.String())
		}
		seen[broker.lastVM]++
	}
	if seen[a.ID] == 0 || seen[b.ID] == 0 {
		t.Errorf("balancing did not cover both backends: a=%d b=%d", seen[a.ID], seen[b.ID])
	}
	if len(seen) != 2 {
		t.Errorf("chose an ineligible backend: seen=%v", seen)
	}
	_ = lb
}

// scriptedBroker returns a configured error for specific backends and a gateway
// success for the rest, so a connect test can exercise per-backend resolve
// failures.
type scriptedBroker struct {
	fail map[uuid.UUID]error
}

func (b *scriptedBroker) ResolveIngress(_ context.Context, vm store.VM, port int) (vms.IngressResult, error) {
	if err := b.fail[vm.ID]; err != nil {
		return vms.IngressResult{}, err
	}
	return vms.IngressResult{
		Transport: "gateway", VMID: vm.ID, VMName: vm.Name, Port: port,
		SplicerAddr: "https://gw.test:9444", SplicerServerName: "node-gw.agents.otherix.local", SessionCred: "cred",
	}, nil
}

// TestConnectTriesRemainingBackendsAfterHardError: a hard (non-ErrIngressUnavailable)
// resolve error on one backend must not fail the whole connect - the handler tries
// the remaining candidates. Backend A always hard-errors, B always resolves; over
// many shuffled iterations the connect must always succeed via B.
func TestConnectTriesRemainingBackendsAfterHardError(t *testing.T) {
	st := newFakeStore()
	owner := uuid.New()
	st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	a := st.seedRunningVM(t, owner, `{"app":"web"}`)
	b := st.seedRunningVM(t, owner, `{"app":"web"}`)
	broker := &scriptedBroker{fail: map[uuid.UUID]error{a.ID: errors.New("transient resolve failure")}}
	h := loadbalancers.New(st, broker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	for i := 0; i < 40; i++ {
		rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d: connect status = %d (a hard error on one backend must not fail the connect), body=%s", i, rec.Code, rec.Body.String())
		}
	}
	_ = b
}

// TestConnectAllHardErrorsReturns500: when EVERY backend hits a hard resolve error
// (not the benign ErrIngressUnavailable), the connect must still surface a 500 -
// the resilience fix must not mask a systemic resolve failure as a benign "no
// reachable backend".
func TestConnectAllHardErrorsReturns500(t *testing.T) {
	st := newFakeStore()
	owner := uuid.New()
	st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	a := st.seedRunningVM(t, owner, `{"app":"web"}`)
	b := st.seedRunningVM(t, owner, `{"app":"web"}`)
	broker := &scriptedBroker{fail: map[uuid.UUID]error{
		a.ID: errors.New("transient resolve failure"),
		b.ID: errors.New("transient resolve failure"),
	}}
	h := loadbalancers.New(st, broker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("all-hard-error connect status = %d, want 500", rec.Code)
	}
}

// TestConnectSurfacesSplicerServerName proves the connect response carries the
// gateway node's identity ServerName (node-<name>.agents.otherix.local) the
// broker resolved, so the client pins the ingress TLS ServerName to the node
// identity rather than the dialed ingress IP. Revert to confirm: drop the
// SplicerServerName copy in Connect and the field goes empty, failing this test.
func TestConnectSurfacesSplicerServerName(t *testing.T) {
	h, st, _ := newConnectTestHandler(t)
	owner := uuid.New()
	st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	st.seedRunningVM(t, owner, `{"app":"web"}`)
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Transport         string `json:"transport"`
		SplicerServerName string `json:"splicer_server_name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode connect body: %v", err)
	}
	if body.Transport != "gateway" {
		t.Fatalf("transport = %q, want gateway", body.Transport)
	}
	if want := "node-gw.agents.otherix.local"; body.SplicerServerName != want {
		t.Errorf("splicer_server_name = %q, want %q", body.SplicerServerName, want)
	}
}

func TestConnectNoEligibleBackends409(t *testing.T) {
	h, st, _ := newConnectTestHandler(t)
	owner := uuid.New()
	st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	st.seedStoppedVM(t, owner, `{"app":"web"}`) // matching but not running
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}
	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "ingress_unavailable" {
		t.Errorf("code = %q, want ingress_unavailable", code)
	}
}

func TestConnectExcludesNonRunningMatchingVM(t *testing.T) {
	h, st, broker := newConnectTestHandler(t)
	owner := uuid.New()
	st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	running := st.seedRunningVM(t, owner, `{"app":"web"}`)
	stopped := st.seedStoppedVM(t, owner, `{"app":"web"}`)
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	for i := 0; i < 20; i++ {
		rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("connect status = %d, body=%s", rec.Code, rec.Body.String())
		}
		if broker.lastVM == stopped.ID {
			t.Fatalf("chose the stopped VM %v", stopped.ID)
		}
		if broker.lastVM != running.ID {
			t.Fatalf("chose an unexpected VM %v, want running %v", broker.lastVM, running.ID)
		}
	}
}

// TestConnectExcludesTransientRuntimeErrorVM gives teeth to the fail-toward-
// exclusion branch in eligibleBackends: when VMRuntimeByID returns a non-
// ErrNotFound (transient/infra) error, the backend must be excluded rather than
// handed out unconfirmed. Here the transient-error VM is the ONLY matching
// candidate, so exclusion leaves zero eligible backends and Connect must answer
// 409 ingress_unavailable. Revert to confirm: if eligibleBackends treated a
// non-NotFound error as "include" (fail open), this VM would be the sole
// candidate, the broker would resolve it, and Connect would return 200 - failing
// this test.
func TestConnectExcludesTransientRuntimeErrorVM(t *testing.T) {
	h, st, broker := newConnectTestHandler(t)
	owner := uuid.New()
	st.seedLB(t, "web", owner, 8080, map[string]string{"app": "web"})
	vm := st.seedRunningVM(t, owner, `{"app":"web"}`)
	st.runtimeErr[vm.ID] = errors.New("etcd unavailable") // transient read failure
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "ingress_unavailable" {
		t.Errorf("code = %q, want ingress_unavailable", code)
	}
	if broker.lastVM != uuid.Nil {
		t.Errorf("broker was handed a backend %v; a transient-error VM must never be brokered", broker.lastVM)
	}
}

func TestConnectCrossOwner404(t *testing.T) {
	h, st, _ := newConnectTestHandler(t)
	st.seedLB(t, "web", uuid.New(), 8080, map[string]string{"app": "web"}) // owned by someone else
	u := &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper}
	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "loadbalancer_not_found" {
		t.Errorf("code = %q, want loadbalancer_not_found", code)
	}
}

func TestConnectUnknownLB404(t *testing.T) {
	h, _, _ := newConnectTestHandler(t)
	u := &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper}
	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/missing/connect", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := errorCode(t, rec); code != "loadbalancer_not_found" {
		t.Errorf("code = %q, want loadbalancer_not_found", code)
	}
}
