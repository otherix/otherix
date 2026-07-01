// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// lbConnectResp mirrors the connect broker response: the ingress coordinates
// plus the chosen backend name.
type lbConnectResp struct {
	Transport   string `json:"transport"`
	VMID        string `json:"vm_id"`
	VMName      string `json:"vm_name"`
	Port        int    `json:"port"`
	SplicerAddr string `json:"splicer_addr"`
	SessionCred string `json:"session_cred"`
	ExpiresAt   string `json:"expires_at"`
}

// TestLoadBalancerConnect_NotIdempotent proves the connect route sits OUTSIDE
// the Idempotency middleware: two POSTs with the SAME Idempotency-Key each
// broker a fresh session credential rather than replaying the first response
// verbatim. Were connect mounted under idem, the second call would replay the
// first (identical session_cred) and freeze balancing - the auth.login/refresh
// carve-out class. The gateway path is used because it mints a fresh, distinct
// session credential per call, giving the replay-vs-fresh assertion teeth.
func TestLoadBalancerConnect_NotIdempotent(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	// A running overlay VM owned by the admin, labelled to match the LB.
	vmName, vmID, netID, nodeID := seedIngressOverlayVM(t, h, admin, adminID)
	labelBackendVM(t, h, vmID, `{"app":"web"}`)
	markVMRunning(t, h, vmID, nodeID)
	convergeGateway(t, h, netID)
	seedSessionCA(t, h)
	_ = vmName

	// Load balancer selecting that VM.
	resp := h.post(t, "/v1/loadbalancers",
		map[string]any{"name": "web", "port": 22, "selector": map[string]string{"app": "web"}}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create lb status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	key := "idem-lb-connect"
	hdr := map[string]string{middleware.HeaderIdempotencyKey: key, "Content-Type": "application/json"}

	first := h.do(t, http.MethodPost, "/v1/loadbalancers/web/connect", nil, admin, hdr)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first connect status = %d, want 200", first.StatusCode)
	}
	var a lbConnectResp
	decodeJSON(t, first, &a)
	if a.Transport != "gateway" {
		t.Fatalf("first transport = %q, want gateway", a.Transport)
	}
	if a.SessionCred == "" {
		t.Fatal("first connect returned empty session_cred")
	}

	// Same key again: connect must NOT replay - a fresh credential is brokered.
	second := h.do(t, http.MethodPost, "/v1/loadbalancers/web/connect", nil, admin, hdr)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second connect status = %d, want 200 (freshly brokered, not a replay)", second.StatusCode)
	}
	var b lbConnectResp
	decodeJSON(t, second, &b)
	if b.SessionCred == "" {
		t.Fatal("second connect returned empty session_cred")
	}
	if a.SessionCred == b.SessionCred {
		t.Errorf("connect replayed the cached response under a repeated Idempotency-Key: "+
			"session_cred identical (%s) - connect must be OUTSIDE the idem middleware", a.SessionCred)
	}
}

// labelBackendVM rewrites the VM row's Labels in place so the load-balancer
// selector matches it. It preserves every other field (and the name index) by
// reading the row first.
func labelBackendVM(t *testing.T, _ *harness, vmID uuid.UUID, labels string) {
	t.Helper()
	ctx := context.Background()
	var vm store.VM
	found, err := sharedEtcdClient.GetJSON(ctx, etcd.Key("vms", vmID.String()), &vm)
	if err != nil || !found {
		t.Fatalf("read vm row: found=%v err=%v", found, err)
	}
	vm.Labels = []byte(labels)
	if err := sharedEtcdClient.PutJSON(ctx, etcd.Key("vms", vmID.String()), vm); err != nil {
		t.Fatalf("write vm labels: %v", err)
	}
}

// markVMRunning upserts a runtime row observing the VM as running on nodeID, so
// the connect handler's eligibility filter admits it as a backend.
func markVMRunning(t *testing.T, h *harness, vmID, nodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: vmID, CurrentNodeID: &nodeID, Phase: store.VmPhaseRunning, ObservedGeneration: 1,
		})
	}); err != nil {
		t.Fatalf("UpsertVMRuntime: %v", err)
	}
}
