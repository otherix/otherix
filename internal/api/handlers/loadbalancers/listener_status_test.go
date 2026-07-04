// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// listenerStatusJSON mirrors one entry of the listeners[] array the lb get view
// carries for a published load balancer.
type listenerStatusJSON struct {
	NodeID     string `json:"node_id"`
	Port       int32  `json:"port"`
	Bound      bool   `json:"bound"`
	Error      string `json:"error"`
	ReportedAt string `json:"reported_at"`
}

// seedPublishedLB inserts a published load balancer row directly (bypassing the
// create handler), publishing it on publishedPort.
func seedPublishedLB(t *testing.T, st *fakeStore, name string, owner uuid.UUID, publishedPort int32) store.LoadBalancer {
	t.Helper()
	lb := st.seedLB(t, name, owner, 8080, map[string]string{"app": name})
	lb.PublishedPort = &publishedPort
	lb.Protocol = "tcp"
	st.byID[lb.ID] = lb
	st.byPublishedPort[publishedPort] = lb.ID
	return lb
}

// TestGetLoadBalancerListenersFresh asserts a published LB with one fresh
// bound=true status row surfaces that row in the lb get view's listeners[].
func TestGetLoadBalancerListenersFresh(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	lb := seedPublishedLB(t, st, "web", u.ID, 8080)
	node := uuid.New()
	fresh := time.Now().UTC().Truncate(time.Second)
	st.seedListenerStatus(lb.ID, store.LBPublishedListenerStatus{
		NodeID: node, Port: 8080, Bound: true, ReportedAt: fresh,
	})

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Listeners []listenerStatusJSON `json:"listeners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Listeners) != 1 {
		t.Fatalf("listeners len = %d, want 1; body=%s", len(view.Listeners), rec.Body.String())
	}
	got := view.Listeners[0]
	if got.NodeID != node.String() {
		t.Errorf("listener node_id = %q, want %q", got.NodeID, node.String())
	}
	if got.Port != 8080 {
		t.Errorf("listener port = %d, want 8080", got.Port)
	}
	if !got.Bound {
		t.Errorf("listener bound = false, want true")
	}
	if got.ReportedAt == "" {
		t.Errorf("listener reported_at = empty, want a timestamp")
	}
}

// TestGetLoadBalancerListenersStaleOmitted asserts a status row older than the
// heartbeat-floored freshness window is treated as absent, so a dead gateway's
// last-reported row does not read as a live listener.
func TestGetLoadBalancerListenersStaleOmitted(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	lb := seedPublishedLB(t, st, "web", u.ID, 8080)
	stale := time.Now().UTC().Add(-(store.HealthCheckHeartbeatFloorSeconds*store.HealthCheckStalenessFactor + 1) * time.Second)
	st.seedListenerStatus(lb.ID, store.LBPublishedListenerStatus{
		NodeID: uuid.New(), Port: 8080, Bound: true, ReportedAt: stale,
	})

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["listeners"]; ok {
		t.Errorf("published LB with only a stale status carried listeners = %v, want omitted", raw["listeners"])
	}
}

// TestGetLoadBalancerUnpublishedNoListeners asserts an unpublished LB carries no
// listeners field at all.
func TestGetLoadBalancerUnpublishedNoListeners(t *testing.T) {
	st := newFakeStore()
	u := devUser()
	router := newRouter(st, u)

	st.seedLB(t, "web", u.ID, 8080, map[string]string{"app": "web"})

	rec := do(t, router, http.MethodGet, "/v1/loadbalancers/web", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["listeners"]; ok {
		t.Errorf("unpublished LB carried listeners = %v, want omitted", raw["listeners"])
	}
}
