// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVMCreate_VerticalSliceRBAC walks the F2 composite-check truth
// table:
//
//   - admin (vm:create=any, template:use=any) creating against another
//     user's private template → 202 (composite check passes via
//     ScopeAny).
//   - developer (vm:create=any, template:use=own) creating against
//     OWN template → 202 (own-match branch).
//   - developer creating against ANOTHER user's private template →
//     404 (no leak; route gate admits, handler-side composite returns
//     errVMTemplateForbidden which projects to not_found).
//   - developer creating against PUBLIC template → 202 (read:public
//     bypass).
//   - viewer (no vm:create) → 403 at the route gate.
//
// The test does not exercise the full vm.create chain — it asserts
// the handler-edge gate behaviour, since that is where the F2 logic
// lives. Idempotency / async lifecycle are covered separately.
func TestVMCreate_VerticalSliceRBAC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())

	// Seed two principals: dev1 (template owner) and dev2 (cross-user
	// developer). Plus a viewer for the role-gate denial branch.
	_, dev1Email, dev1PW := seedUser(t, ctx, v.store, "developer")
	dev2ID, dev2Email, dev2PW := seedUser(t, ctx, v.store, "developer")
	_, viewerEmail, viewerPW := seedUser(t, ctx, v.store, "viewer")

	dev1Token := loginUser(t, v.cpServer.URL, dev1Email, dev1PW)
	dev2Token := loginUser(t, v.cpServer.URL, dev2Email, dev2PW)
	viewerToken := loginUser(t, v.cpServer.URL, viewerEmail, viewerPW)

	// dev2 owns a private template — for the "own template" branch.
	dev2OwnTpl := seedTemplateForVM(t, ctx, v, dev2ID, "vm-rbac-dev2-own", 0xa6, "private")

	// dev1 owns a private template — for the "cross-user" branch.
	dev1OwnTpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-rbac-admin-priv", 0xb7, "private")

	// public template — for the read:public bypass branch.
	publicTpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-rbac-public", 0xc8, "public")

	mkBody := func(tplName string) vmCreateBody {
		return vmCreateBody{
			Name:     "rbac-vm-" + uuid.NewString()[:8],
			Template: tplName,
			Pool:     v.pool.ID.String(),
			VCPUs:    1,
			MemoryMB: 512,
		}
	}

	cases := []struct {
		name       string
		token      string
		body       vmCreateBody
		wantStatus int
	}{
		{"admin + cross-user private → 202", v.adminToken, mkBody(dev2OwnTpl.Name), http.StatusAccepted},
		{"developer + own private → 202", dev2Token, mkBody(dev2OwnTpl.Name), http.StatusAccepted},
		{"developer + cross-user private → 404", dev2Token, mkBody(dev1OwnTpl.Name), http.StatusNotFound},
		{"developer + public → 202 (read:public bypass)", dev1Token, mkBody(publicTpl.Name), http.StatusAccepted},
		{"viewer → 403 at route gate", viewerToken, mkBody(publicTpl.Name), http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := v.createVMRaw(t, ctx, tc.body, tc.token, "")
			if status != tc.wantStatus {
				t.Errorf("status = %d, body = %s, want %d", status, body, tc.wantStatus)
			}
		})
	}
}
