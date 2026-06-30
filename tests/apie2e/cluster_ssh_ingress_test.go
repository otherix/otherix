// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// TestClusterSSHIngress round-trips the cluster SSH-ingress master switch and
// DNS suffix through the real HTTP edge: an unconfigured cluster reports
// enabled=false / empty suffix, a PUT persists both, enabling without a
// suffix and a malformed suffix are rejected, and a non-admin caller is
// gated by cluster:manage.
func TestClusterSSHIngress(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Unconfigured -> 200 enabled=false, empty suffix.
	resp := h.get(t, "/v1/cluster/ssh-ingress", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get-unset status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Enabled       bool   `json:"enabled"`
		ClusterSuffix string `json:"cluster_suffix"`
	}
	decodeJSON(t, resp, &got)
	if got.Enabled || got.ClusterSuffix != "" {
		t.Errorf("get-unset = %+v, want {false, \"\"}", got)
	}

	// Enable with a suffix -> 200, echoes the values.
	resp = h.put(t, "/v1/cluster/ssh-ingress",
		map[string]any{"enabled": true, "cluster_suffix": "ssh.otherix.local"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	got.Enabled, got.ClusterSuffix = false, ""
	decodeJSON(t, resp, &got)
	if !got.Enabled || got.ClusterSuffix != "ssh.otherix.local" {
		t.Errorf("put = %+v, want {true, ssh.otherix.local}", got)
	}

	// GET now reflects the persisted values.
	resp = h.get(t, "/v1/cluster/ssh-ingress", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	got.Enabled, got.ClusterSuffix = false, ""
	decodeJSON(t, resp, &got)
	if !got.Enabled || got.ClusterSuffix != "ssh.otherix.local" {
		t.Errorf("get = %+v, want {true, ssh.otherix.local}", got)
	}

	// Enabling without a suffix -> 400 validation_failed.
	resp = h.put(t, "/v1/cluster/ssh-ingress", map[string]any{"enabled": true}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enable-no-suffix status = %d, want 400", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &env)
	if env.Error.Code != "validation_failed" {
		t.Errorf("enable-no-suffix code = %q, want validation_failed", env.Error.Code)
	}

	// Malformed suffix -> 400 validation_failed.
	resp = h.put(t, "/v1/cluster/ssh-ingress",
		map[string]any{"enabled": true, "cluster_suffix": "BAD_SUFFIX!"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-suffix status = %d, want 400", resp.StatusCode)
	}
	env.Error.Code = ""
	decodeJSON(t, resp, &env)
	if env.Error.Code != "validation_failed" {
		t.Errorf("bad-suffix code = %q, want validation_failed", env.Error.Code)
	}

	// Disable -> 200 enabled=false.
	resp = h.put(t, "/v1/cluster/ssh-ingress", map[string]any{"enabled": false}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable status = %d, want 200", resp.StatusCode)
	}
	got.Enabled, got.ClusterSuffix = true, "x"
	decodeJSON(t, resp, &got)
	if got.Enabled {
		t.Errorf("disable enabled = %v, want false", got.Enabled)
	}

	// PUT as a non-admin (developer) -> 403 permission_denied.
	dev, _ := loginAs(t, h, auth.RoleDeveloper)
	resp = h.put(t, "/v1/cluster/ssh-ingress",
		map[string]any{"enabled": true, "cluster_suffix": "ssh.otherix.local"}, dev)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("put-as-developer status = %d, want 403", resp.StatusCode)
	}
	env.Error.Code = ""
	decodeJSON(t, resp, &env)
	if env.Error.Code != "permission_denied" {
		t.Errorf("put-as-developer code = %q, want permission_denied", env.Error.Code)
	}
}

// TestVMCreate_SSHIngressEnabledPersists confirms a VM created with
// ssh_ingress_enabled=true persists the per-VM opt-in on the VM row.
func TestVMCreate_SSHIngressEnabledPersists(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)

	body := vmCreateBody(map[string]any{
		"name":                "ssh-vm",
		"pool":                poolName,
		"ssh_ingress_enabled": true,
	})
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	vm, err := h.store.VMByName(context.Background(), "ssh-vm")
	if err != nil {
		t.Fatalf("VMByName: %v", err)
	}
	if !vm.SSHIngressEnabled {
		t.Errorf("persisted SSHIngressEnabled = false, want true")
	}
}
