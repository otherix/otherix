// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

const (
	r2h1UserData = "#cloud-config\nhostname: secret-vm\nssh_authorized_keys: [\"ssh-ed25519 AAAA\"]\n"
	r2h1NetCfg   = "version: 2\nethernets:\n  eth0:\n    dhcp4: true\n"
	// r2h1SHA pins the image digest so image_sha256 (omitempty, inventory not
	// secret) is present for every caller - the gating assertion below relies
	// on it surviving the strip.
	r2h1SHA = "2222222222222222222222222222222222222222222222222222222222222222"
)

// secretFields are the VM view fields gated to vm:console holders (R2-H1). Kept in one place so
// every gating assertion checks the same set.
var secretFields = []string{"user_data", "network_config", "image_url"}

// createVMWithSecrets creates a VM (owned by the principal behind ownerToken) carrying
// user_data + network_config, and returns its name. The pool is admin-provisioned.
func createVMWithSecrets(t *testing.T, h *harness, ownerToken, poolName string) string {
	t.Helper()
	body := vmCreateBody(map[string]any{
		"pool":           poolName,
		"image_sha256":   r2h1SHA,
		"user_data":      r2h1UserData,
		"network_config": r2h1NetCfg,
	})
	name := body["name"].(string)
	resp := h.post(t, "/v1/vms", body, ownerToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	return name
}

// assertSecretFields fetches the VM as token and checks whether the three secret-bearing
// fields are present (want=true) or absent (want=false). Non-secret inventory (name,
// image_sha256) must always be present.
func assertSecretFields(t *testing.T, h *harness, token, vmName string, want bool) {
	t.Helper()
	resp := h.get(t, "/v1/vms/"+vmName, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", resp.StatusCode)
	}
	var raw map[string]any
	decodeJSON(t, resp, &raw)
	if _, ok := raw["name"]; !ok {
		t.Errorf("inventory field name missing - view broken")
	}
	if _, ok := raw["image_sha256"]; !ok {
		t.Errorf("inventory field image_sha256 missing - view broken")
	}
	for _, f := range secretFields {
		_, present := raw[f]
		if present != want {
			t.Errorf("field %q present=%v, want %v (token-scoped secret gating)", f, present, want)
		}
	}
}

// TestVMSecretFieldsGatedToConsoleHolders is the R2-H1 seam test: the cloud-init payloads and
// image_url are visible only to a caller who holds vm:console on that VM (owner, admin, operator);
// stripped for a viewer and for a non-owning developer.
func TestVMSecretFieldsGatedToConsoleHolders(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	operator, _ := loginAs(t, h, auth.RoleOperator)
	owner, _ := loginAs(t, h, auth.RoleDeveloper)
	otherDev, _ := loginAs(t, h, auth.RoleDeveloper)
	viewer, _ := loginAs(t, h, auth.RoleViewer)
	poolName := schedulableFixture(t, h, adminID)

	vmName := createVMWithSecrets(t, h, owner, poolName)

	// console-holders see the secrets.
	assertSecretFields(t, h, owner, vmName, true)    // developer, own VM (console own)
	assertSecretFields(t, h, admin, vmName, true)    // console any
	assertSecretFields(t, h, operator, vmName, true) // console any
	// non-console callers do not.
	assertSecretFields(t, h, otherDev, vmName, false) // developer, foreign VM (console own fails)
	assertSecretFields(t, h, viewer, vmName, false)   // viewer holds no vm:console
}

// TestVMSecretFieldsGatedInList asserts the same gating on the list projection: a viewer's page
// has the secret fields stripped on every row, an admin's page retains them.
func TestVMSecretFieldsGatedInList(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	viewer, _ := loginAs(t, h, auth.RoleViewer)
	poolName := schedulableFixture(t, h, adminID)

	_ = createVMWithSecrets(t, h, admin, poolName)

	adminList := listVMRaw(t, h, admin)
	if len(adminList) == 0 {
		t.Fatalf("admin list empty")
	}
	for _, row := range adminList {
		for _, f := range secretFields {
			if _, ok := row[f]; !ok {
				t.Errorf("admin list row missing %q, want present", f)
			}
		}
	}
	viewerList := listVMRaw(t, h, viewer)
	if len(viewerList) == 0 {
		t.Fatalf("viewer list empty")
	}
	for _, row := range viewerList {
		for _, f := range secretFields {
			if _, ok := row[f]; ok {
				t.Errorf("viewer list row leaked %q", f)
			}
		}
	}
}

// listVMRaw GETs /v1/vms as token and returns the data rows as raw maps (so an absent key is
// distinguishable from null).
func listVMRaw(t *testing.T, h *harness, token string) []map[string]any {
	t.Helper()
	resp := h.get(t, "/v1/vms", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list vms status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	decodeJSON(t, resp, &env)
	return env.Data
}
