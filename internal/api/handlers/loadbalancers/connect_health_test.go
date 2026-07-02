// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers_test

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// healthTestInterval is the effective probe interval the tests pin so the
// staleness window (HealthCheckStalenessFactor x interval) is deterministic.
const healthTestInterval = 10

// seedLBWithInterval seeds a load balancer and pins its health-check interval so
// the staleness window is a known value.
func seedLBWithInterval(t *testing.T, st *fakeStore, name string, owner uuid.UUID, port int32, selector map[string]string) store.LoadBalancer {
	t.Helper()
	lb := st.seedLB(t, name, owner, port, selector)
	lb.HealthCheck = store.LoadBalancerHealthCheck{IntervalSeconds: healthTestInterval}
	st.byID[lb.ID] = lb
	return lb
}

// sortedIDs returns the VM ids in a stable order for cmp.Diff.
func sortedIDs(vms []store.VM) []uuid.UUID {
	out := make([]uuid.UUID, len(vms))
	for i, vm := range vms {
		out[i] = vm.ID
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func sortIDs(ids []uuid.UUID) []uuid.UUID {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

// TestEligibleBackendsExcludesFreshUnhealthy proves the subtractive rule: with
// three selector-matched running backends, a fresh Healthy==false record drops
// that backend, while a backend with no record at all degrades to phase==running
// and stays included.
func TestEligibleBackendsExcludesFreshUnhealthy(t *testing.T) {
	h, st, _ := newConnectTestHandler(t)
	owner := uuid.New()
	lb := seedLBWithInterval(t, st, "web", owner, 8080, map[string]string{"app": "web"})
	vmA := st.seedRunningVM(t, owner, `{"app":"web"}`)
	vmB := st.seedRunningVM(t, owner, `{"app":"web"}`)
	vmC := st.seedRunningVM(t, owner, `{"app":"web"}`)

	now := time.Now()
	st.seedHealth(lb.ID, vmA.ID, true, now)  // fresh healthy -> include
	st.seedHealth(lb.ID, vmB.ID, false, now) // fresh unhealthy -> exclude
	// vmC has no record -> degrade to phase==running -> include.

	got, err := h.EligibleBackends(context.Background(), lb)
	if err != nil {
		t.Fatalf("EligibleBackends() error = %v", err)
	}
	want := sortIDs([]uuid.UUID{vmA.ID, vmC.ID})
	if diff := cmp.Diff(want, sortedIDs(got)); diff != "" {
		t.Errorf("EligibleBackends() ids mismatch (-want +got):\n%s", diff)
	}
}

// TestEligibleBackendsIncludesStaleUnhealthy proves the freshness guard: a
// Healthy==false record older than HealthCheckStalenessFactor x interval must
// NOT darken the backend - a stale record degrades to phase==running.
func TestEligibleBackendsIncludesStaleUnhealthy(t *testing.T) {
	h, st, _ := newConnectTestHandler(t)
	owner := uuid.New()
	lb := seedLBWithInterval(t, st, "web", owner, 8080, map[string]string{"app": "web"})
	vmA := st.seedRunningVM(t, owner, `{"app":"web"}`)

	// Reported just past the staleness window (3 x 10s = 30s).
	stale := time.Now().Add(-(healthTestInterval*store.HealthCheckStalenessFactor + 1) * time.Second)
	st.seedHealth(lb.ID, vmA.ID, false, stale)

	got, err := h.EligibleBackends(context.Background(), lb)
	if err != nil {
		t.Fatalf("EligibleBackends() error = %v", err)
	}
	want := []uuid.UUID{vmA.ID}
	if diff := cmp.Diff(want, sortedIDs(got)); diff != "" {
		t.Errorf("EligibleBackends() ids mismatch (-want +got):\n%s", diff)
	}
}

// TestEligibleBackendsHealthReadFailureIncludes gives teeth to the degrade
// branch: when the health read fails, a backend that WOULD be excluded by a
// fresh unhealthy record stays in, because a broken probe pipeline must not dark
// the LB.
func TestEligibleBackendsHealthReadFailureIncludes(t *testing.T) {
	h, st, _ := newConnectTestHandler(t)
	owner := uuid.New()
	lb := seedLBWithInterval(t, st, "web", owner, 8080, map[string]string{"app": "web"})
	vmA := st.seedRunningVM(t, owner, `{"app":"web"}`)

	st.seedHealth(lb.ID, vmA.ID, false, time.Now()) // fresh unhealthy...
	st.healthErr[lb.ID] = context.DeadlineExceeded  // ...but the read fails -> degrade

	got, err := h.EligibleBackends(context.Background(), lb)
	if err != nil {
		t.Fatalf("EligibleBackends() error = %v", err)
	}
	want := []uuid.UUID{vmA.ID}
	if diff := cmp.Diff(want, sortedIDs(got)); diff != "" {
		t.Errorf("EligibleBackends() ids mismatch (-want +got):\n%s", diff)
	}
}

// TestConnectAllFreshUnhealthy409 drives the real Connect handler: when every
// selector-matched running backend is fresh-confirmed unhealthy, eligibility is
// empty and Connect answers 409 ingress_unavailable (the honest all-down case).
func TestConnectAllFreshUnhealthy409(t *testing.T) {
	h, st, broker := newConnectTestHandler(t)
	owner := uuid.New()
	lb := seedLBWithInterval(t, st, "web", owner, 8080, map[string]string{"app": "web"})
	now := time.Now()
	for i := 0; i < 3; i++ {
		vm := st.seedRunningVM(t, owner, `{"app":"web"}`)
		st.seedHealth(lb.ID, vm.ID, false, now)
	}
	u := &auth.User{ID: owner, Role: auth.RoleDeveloper}

	rec := doAuthedRequest(t, h.Connect, u, http.MethodPost, "/v1/loadbalancers/web/connect", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "ingress_unavailable" {
		t.Errorf("code = %q, want ingress_unavailable", code)
	}
	if broker.lastVM != uuid.Nil {
		t.Errorf("broker was handed a backend %v; all-unhealthy must broker nothing", broker.lastVM)
	}
}
