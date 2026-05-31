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

func TestClusterMembersListAsAdmin(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.get(t, "/v1/cluster/members", adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/cluster/members status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			PeerURLs   []string `json:"peer_urls"`
			ClientURLs []string `json:"client_urls"`
			IsLearner  bool     `json:"is_learner"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &body)

	if len(body.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(body.Data))
	}
	m := body.Data[0]
	if m.ID != "1a2b" {
		t.Errorf("data[0].id = %q, want %q", m.ID, "1a2b")
	}
	if m.Name != "n0" {
		t.Errorf("data[0].name = %q, want %q", m.Name, "n0")
	}
	if m.IsLearner {
		t.Errorf("data[0].is_learner = true, want false")
	}
}

func TestClusterMembersListForbiddenForNonAdmin(t *testing.T) {
	h := newE2E(t)
	viewerTok, _ := loginAs(t, h, auth.RoleViewer)

	resp := h.get(t, "/v1/cluster/members", viewerTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer GET /v1/cluster/members status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	developerTok, _ := loginAs(t, h, auth.RoleDeveloper)
	resp2 := h.get(t, "/v1/cluster/members", developerTok)
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("developer GET /v1/cluster/members status = %d, want 403", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}

func TestClusterMemberRemoveAsAdmin(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.delete(t, "/v1/cluster/members/1a2b", adminTok)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /v1/cluster/members/1a2b status = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	ids := h.membership.removedIDs()
	if len(ids) != 1 {
		t.Fatalf("removedIDs len = %d, want 1", len(ids))
	}
	if ids[0] != uint64(0x1a2b) {
		t.Errorf("removedIDs[0] = %x, want 1a2b", ids[0])
	}
}

func TestClusterMemberRemoveForbiddenForNonAdmin(t *testing.T) {
	h := newE2E(t)
	viewerTok, _ := loginAs(t, h, auth.RoleViewer)

	resp := h.delete(t, "/v1/cluster/members/1a2b", viewerTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("viewer DELETE /v1/cluster/members/1a2b status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if ids := h.membership.removedIDs(); len(ids) != 0 {
		t.Errorf("removedIDs after RBAC block = %v, want empty", ids)
	}
}

func TestClusterMemberRemoveBadID(t *testing.T) {
	h := newE2E(t)
	adminTok, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.delete(t, "/v1/cluster/members/not-hex", adminTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE /v1/cluster/members/not-hex status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
