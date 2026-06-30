// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// sshGrantView is the decode target for the public SSH-grant projection. The
// token-hash is never surfaced; token is present only on the create response.
type sshGrantView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	RecipientLabel string `json:"recipient_label"`
	CreatedBy      string `json:"created_by"`
	VMs            []struct {
		VMName string `json:"vm_name"`
		Login  string `json:"login"`
	} `json:"vms"`
	ExpiresAt *string `json:"expires_at"`
	Revoked   bool    `json:"revoked"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	Token     string  `json:"token"`
}

// TestSSHGrantLifecycle drives the full operator-facing grant CRUD: create
// (token returned once), get (token omitted), add-vm, remove-vm, revoke, list.
func TestSSHGrantLifecycle(t *testing.T) {
	h := newE2E(t)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	vm1, _ := seedOwnedVM(t, h.store, opID)
	vm2, _ := seedOwnedVM(t, h.store, opID)

	// Create.
	resp := h.post(t, "/v1/ssh-grants", map[string]any{
		"name":            "alice-access",
		"recipient_label": "Alice",
		"vms":             []map[string]string{{"vm_name": vm1, "login": "ubuntu"}},
		"ttl":             "24h",
	}, opTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created sshGrantView
	decodeJSON(t, resp, &created)
	if !strings.HasPrefix(created.Token, "otx_sshgrant_") {
		t.Fatalf("create token = %q, want otx_sshgrant_ prefix", created.Token)
	}
	if created.ID == "" {
		t.Fatal("create returned empty id")
	}
	if created.CreatedBy != opID.String() {
		t.Errorf("created_by = %q, want %q", created.CreatedBy, opID)
	}
	if len(created.VMs) != 1 || created.VMs[0].VMName != vm1 || created.VMs[0].Login != "ubuntu" {
		t.Fatalf("create vms = %+v, want one %s/ubuntu", created.VMs, vm1)
	}
	if created.ExpiresAt == nil {
		t.Error("create expires_at = nil, want a timestamp from ttl")
	}

	// Get omits the token.
	resp = h.get(t, "/v1/ssh-grants/"+created.ID, opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got sshGrantView
	decodeJSON(t, resp, &got)
	if got.Token != "" {
		t.Errorf("get surfaced token %q, want empty", got.Token)
	}
	if len(got.VMs) != 1 {
		t.Fatalf("get vms = %+v, want one", got.VMs)
	}

	// Add a VM.
	resp = h.post(t, "/v1/ssh-grants/"+created.ID+"/vms",
		map[string]string{"vm_name": vm2, "login": "root"}, opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add-vm status = %d, want 200", resp.StatusCode)
	}
	var added sshGrantView
	decodeJSON(t, resp, &added)
	if len(added.VMs) != 2 {
		t.Fatalf("add-vm vms = %+v, want two", added.VMs)
	}

	// Remove a VM.
	resp = h.delete(t, "/v1/ssh-grants/"+created.ID+"/vms/"+vm2, opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove-vm status = %d, want 200", resp.StatusCode)
	}
	var removed sshGrantView
	decodeJSON(t, resp, &removed)
	if len(removed.VMs) != 1 || removed.VMs[0].VMName != vm1 {
		t.Fatalf("remove-vm vms = %+v, want one %s", removed.VMs, vm1)
	}

	// Revoke.
	resp = h.post(t, "/v1/ssh-grants/"+created.ID+"/revoke", nil, opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", resp.StatusCode)
	}
	var revoked sshGrantView
	decodeJSON(t, resp, &revoked)
	if !revoked.Revoked {
		t.Error("revoke did not set revoked=true")
	}

	// List includes the grant.
	resp = h.get(t, "/v1/ssh-grants", opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Data []sshGrantView `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].ID != created.ID {
		t.Fatalf("list data = %+v, want the one created grant", list.Data)
	}
	if list.Data[0].Token != "" {
		t.Errorf("list surfaced token, want empty")
	}
}

// TestSSHGrantLoginValidation asserts the grant create and add-vm paths
// reject a login that is not a safe SSH principal with 400 validation_failed,
// mirroring the cert-mint sanitize rule, and accept a well-formed login.
func TestSSHGrantLoginValidation(t *testing.T) {
	h := newE2E(t)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	vm, _ := seedOwnedVM(t, h.store, opID)

	badLogins := []string{
		"root;rm",               // shell metacharacter
		"Foo Bar",               // space + uppercase
		"   ",                   // empty after trim
		strings.Repeat("a", 40), // over the 32-char cap
		"0day",                  // leading digit
	}
	for _, login := range badLogins {
		resp := h.post(t, "/v1/ssh-grants", map[string]any{
			"name": "bad-" + login,
			"vms":  []map[string]string{{"vm_name": vm, "login": login}},
		}, opTok)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create with login %q status = %d, want 400", login, resp.StatusCode)
		}
	}

	// A valid login creates the grant.
	resp := h.post(t, "/v1/ssh-grants", map[string]any{
		"name": "good-create",
		"vms":  []map[string]string{{"vm_name": vm, "login": "deploy"}},
	}, opTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with login deploy status = %d, want 201", resp.StatusCode)
	}
	var created sshGrantView
	decodeJSON(t, resp, &created)

	// add-vm enforces the same rule.
	vm2, _ := seedOwnedVM(t, h.store, opID)
	for _, login := range badLogins {
		resp := h.post(t, "/v1/ssh-grants/"+created.ID+"/vms",
			map[string]string{"vm_name": vm2, "login": login}, opTok)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("add-vm with login %q status = %d, want 400", login, resp.StatusCode)
		}
	}
	resp = h.post(t, "/v1/ssh-grants/"+created.ID+"/vms",
		map[string]string{"vm_name": vm2, "login": "ubuntu"}, opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add-vm with login ubuntu status = %d, want 200", resp.StatusCode)
	}
}

// TestSSHGrantNameConflict asserts a duplicate name returns 409 conflict.
func TestSSHGrantNameConflict(t *testing.T) {
	h := newE2E(t)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	vm, _ := seedOwnedVM(t, h.store, opID)

	body := map[string]any{"name": "dup", "vms": []map[string]string{{"vm_name": vm, "login": "ubuntu"}}}
	if resp := h.post(t, "/v1/ssh-grants", body, opTok); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}
	resp := h.post(t, "/v1/ssh-grants", body, opTok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate-name create status = %d, want 409", resp.StatusCode)
	}
}

// TestSSHGrantRBAC asserts the vm:ssh-grant own/any/none matrix end to end:
// viewer 403, developer own-VM create 201, developer foreign-VM create 403,
// developer reading a foreign grant 404 (no existence leak).
func TestSSHGrantRBAC(t *testing.T) {
	h := newE2E(t)
	opTok, opID := loginAs(t, h, auth.RoleOperator)
	devTok, devID := loginAs(t, h, auth.RoleDeveloper)
	viewerTok, _ := loginAs(t, h, auth.RoleViewer)

	ownVM, _ := seedOwnedVM(t, h.store, devID)
	foreignVM, _ := seedOwnedVM(t, h.store, opID)

	// Viewer holds no vm:ssh-grant -> 403 at the middleware.
	resp := h.post(t, "/v1/ssh-grants",
		map[string]any{"name": "v", "vms": []map[string]string{{"vm_name": ownVM, "login": "ubuntu"}}}, viewerTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create status = %d, want 403", resp.StatusCode)
	}

	// Developer creating a grant on a VM they own -> 201.
	resp = h.post(t, "/v1/ssh-grants",
		map[string]any{"name": "dev-own", "vms": []map[string]string{{"vm_name": ownVM, "login": "ubuntu"}}}, devTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("developer own-VM create status = %d, want 201", resp.StatusCode)
	}

	// Developer creating a grant on a VM owned by someone else -> 403
	// (the VM is visible via vm:read=any, but vm:ssh-grant is own-scoped).
	resp = h.post(t, "/v1/ssh-grants",
		map[string]any{"name": "dev-foreign", "vms": []map[string]string{{"vm_name": foreignVM, "login": "ubuntu"}}}, devTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("developer foreign-VM create status = %d, want 403", resp.StatusCode)
	}

	// Operator creates a grant; the developer must not see it (404, not 403).
	resp = h.post(t, "/v1/ssh-grants",
		map[string]any{"name": "op-grant", "vms": []map[string]string{{"vm_name": foreignVM, "login": "ubuntu"}}}, opTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("operator create status = %d, want 201", resp.StatusCode)
	}
	var op sshGrantView
	decodeJSON(t, resp, &op)
	resp = h.get(t, "/v1/ssh-grants/"+op.ID, devTok)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("developer reading operator grant status = %d, want 404", resp.StatusCode)
	}
}
