// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"slices"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// TestLoadBalancerPublishLifecycle: operator creates a published LB, GET
// surfaces the fields, PATCH unpublishes (published_port 0), and a duplicate
// published_port is rejected 409.
func TestLoadBalancerPublishLifecycle(t *testing.T) {
	h := newE2E(t)
	op, _ := loginAs(t, h, auth.RoleOperator)

	create := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "web-db", "port": 5432, "selector": map[string]string{"app": "db"},
		"published_port": 5432, "source_cidrs": []string{"203.0.113.0/24"},
	}, op)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create published = %d, want 201", create.StatusCode)
	}
	create.Body.Close()

	// GET reads the publish fields back: a serialization regression that dropped
	// them from the view would slip past a create-only check.
	get := h.get(t, "/v1/loadbalancers/web-db", op)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get published = %d, want 200", get.StatusCode)
	}
	var got struct {
		PublishedPort int      `json:"published_port"`
		SourceCIDRs   []string `json:"source_cidrs"`
		Protocol      string   `json:"protocol"`
	}
	decodeJSON(t, get, &got)
	if got.PublishedPort != 5432 {
		t.Errorf("published_port = %d, want 5432", got.PublishedPort)
	}
	if !slices.Contains(got.SourceCIDRs, "203.0.113.0/24") {
		t.Errorf("source_cidrs = %v, want to contain 203.0.113.0/24", got.SourceCIDRs)
	}
	if got.Protocol != "tcp" {
		t.Errorf("protocol = %q, want tcp", got.Protocol)
	}

	// Duplicate published_port -> 409.
	dup := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "web-db2", "port": 5432, "selector": map[string]string{"app": "db"},
		"published_port": 5432,
	}, op)
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("dup published_port = %d, want 409", dup.StatusCode)
	}
	dup.Body.Close()

	// Unpublish via PATCH published_port: 0.
	patch := h.patch(t, "/v1/loadbalancers/web-db", map[string]any{"published_port": 0}, op)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("unpublish = %d, want 200", patch.StatusCode)
	}
	patch.Body.Close()

	// After unpublish, web-db2 may claim 5432.
	reuse := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "web-db2", "port": 5432, "selector": map[string]string{"app": "db"},
		"published_port": 5432,
	}, op)
	if reuse.StatusCode != http.StatusCreated {
		t.Fatalf("reuse freed port = %d, want 201", reuse.StatusCode)
	}
	reuse.Body.Close()
}

// TestLoadBalancerPublishForbiddenForDeveloper: a developer creating a
// published LB is 403 (lacks loadbalancer:publish), but an unpublished LB is 201.
func TestLoadBalancerPublishForbiddenForDeveloper(t *testing.T) {
	h := newE2E(t)
	dev, _ := loginAs(t, h, auth.RoleDeveloper)

	forbidden := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "d1", "port": 80, "selector": map[string]string{"app": "d"}, "published_port": 8080,
	}, dev)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("dev published create = %d, want 403", forbidden.StatusCode)
	}
	forbidden.Body.Close()

	ok := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "d2", "port": 80, "selector": map[string]string{"app": "d"},
	}, dev)
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("dev unpublished create = %d, want 201", ok.StatusCode)
	}
	ok.Body.Close()
}

// TestLoadBalancerPublishGateCoversSourceCIDRs verifies the publish gate covers
// the whole exposure surface, not published_port alone: a developer who owns a
// load balancer that an operator later published must NOT be able to strip its
// source-CIDR allowlist without loadbalancer:publish.
func TestLoadBalancerPublishGateCoversSourceCIDRs(t *testing.T) {
	h := newE2E(t)
	dev, _ := loginAs(t, h, auth.RoleDeveloper)
	op, _ := loginAs(t, h, auth.RoleOperator)

	// Developer creates an unpublished LB they own.
	create := h.post(t, "/v1/loadbalancers", map[string]any{
		"name": "dev-owned", "port": 5432, "selector": map[string]string{"app": "db"},
	}, dev)
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("dev create = %d, want 201", create.StatusCode)
	}
	create.Body.Close()

	// Operator publishes the developer-owned LB with an allowlist. The operator
	// holds loadbalancer:update at any scope, so the ownership check passes; the
	// developer remains the owner and is unchanged by publishing.
	pub := h.patch(t, "/v1/loadbalancers/dev-owned", map[string]any{
		"published_port": 5432, "source_cidrs": []string{"203.0.113.0/24"},
	}, op)
	if pub.StatusCode != http.StatusOK {
		t.Fatalf("operator publish = %d, want 200", pub.StatusCode)
	}
	pub.Body.Close()

	// Developer (owner, but no publish perm) tries to strip the allowlist -> 403.
	strip := h.patch(t, "/v1/loadbalancers/dev-owned", map[string]any{
		"source_cidrs": []string{},
	}, dev)
	if strip.StatusCode != http.StatusForbidden {
		t.Fatalf("dev strip source_cidrs = %d, want 403", strip.StatusCode)
	}
	strip.Body.Close()
}
