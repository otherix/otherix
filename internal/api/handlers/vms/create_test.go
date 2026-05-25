// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// TestCheckTemplateUseAccess covers the F2 composite truth table:
// scope=any unblocks; scope=own + match unblocks; public + read:public
// unblocks; everything else returns errVMTemplateForbidden.
func TestCheckTemplateUseAccess(t *testing.T) {
	t.Parallel()

	publicTemplate := store.Template{ID: uuid.New(), OwnerID: uuid.New(), Visibility: "public"}
	privateOwned := func(owner uuid.UUID) store.Template {
		return store.Template{ID: uuid.New(), OwnerID: owner, Visibility: "private"}
	}
	privateOther := store.Template{ID: uuid.New(), OwnerID: uuid.New(), Visibility: "private"}
	mkUser := func(role auth.Role) *auth.User {
		return &auth.User{ID: uuid.New(), Role: role}
	}

	cases := []struct {
		name   string
		caller *auth.User
		tmpl   store.Template
		want   error
	}{
		{
			name:   "admin (any) + private cross-user → ok",
			caller: mkUser(auth.RoleAdmin),
			tmpl:   privateOther,
			want:   nil,
		},
		{
			name:   "operator (any) + public → ok",
			caller: mkUser(auth.RoleOperator),
			tmpl:   publicTemplate,
			want:   nil,
		},
		{
			name: "developer (own) + own private → ok",
			caller: func() *auth.User {
				u := mkUser(auth.RoleDeveloper)
				return u
			}(),
			tmpl: store.Template{},
			want: nil,
		},
		{
			name:   "developer + cross-user private → forbidden",
			caller: mkUser(auth.RoleDeveloper),
			tmpl:   privateOther,
			want:   errVMTemplateForbidden,
		},
		{
			name:   "developer + public template → ok (read:public bypass)",
			caller: mkUser(auth.RoleDeveloper),
			tmpl:   publicTemplate,
			want:   nil,
		},
		{
			// Viewer holds template:read:public, so the public-bypass
			// branch admits the helper even though they cannot act on
			// vm:create. The route gate (RequirePermission(PermVMCreate))
			// blocks the actual call upstream of this helper.
			name:   "viewer + public → allowed by helper (route gate blocks upstream)",
			caller: mkUser(auth.RoleViewer),
			tmpl:   publicTemplate,
			want:   nil,
		},
		{
			name:   "viewer + private cross-user → forbidden",
			caller: mkUser(auth.RoleViewer),
			tmpl:   privateOther,
			want:   errVMTemplateForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpl := tc.tmpl
			// Wire the developer-owns-private case through the caller
			// id so the comparison ScopeOwn+OwnerID uses real values.
			if tc.name == "developer (own) + own private → ok" {
				tmpl = privateOwned(tc.caller.ID)
			}
			got := checkTemplateUseAccess(tc.caller, tmpl)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("checkTemplateUseAccess(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestValidateCreateRequest covers the field-level invariants the
// API edge enforces before any DB work. Each row exercises one
// rejection branch; happy-path vetting lives in the handler-level
// integration test that needs a real store.
//
// The validator does not reject non-UUID strings on the `template`
// or `pool` fields - the resolver layer is responsible for both
// UUID-rejection (template: name-only) and the multi-instance carve-
// out (pool: polymorphic), and surfaces 404 / 400 as appropriate.
// This unit test only covers the field-level edge.
func TestValidateCreateRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  vmCreateRequest
		ok   bool
	}{
		{
			name: "happy path",
			req: vmCreateRequest{
				Name:     "demo",
				Template: "ubuntu-jammy",
				Pool:     "default",
				VCPUs:    2,
				MemoryMB: 2048,
			},
			ok: true,
		},
		{
			name: "empty name",
			req:  vmCreateRequest{Template: "ubuntu", Pool: "default", VCPUs: 2, MemoryMB: 1024},
		},
		{
			name: "name too long",
			req: vmCreateRequest{
				Name:     string(make([]byte, 256)),
				Template: "ubuntu", Pool: "default", VCPUs: 2, MemoryMB: 1024,
			},
		},
		{
			name: "empty template",
			req:  vmCreateRequest{Name: "x", Template: "", Pool: "default", VCPUs: 2, MemoryMB: 1024},
		},
		{
			// Empty pool is admitted - the handler will substitute the
			// cluster default at runtime, or return 400
			// default_pool_not_set when no default is configured. The
			// field-level validator therefore accepts the shape.
			name: "empty pool ok (cluster default fallback)",
			req:  vmCreateRequest{Name: "x", Template: "ubuntu", Pool: "", VCPUs: 2, MemoryMB: 1024},
			ok:   true,
		},
		{
			name: "vcpus too small",
			req:  vmCreateRequest{Name: "x", Template: "ubuntu", Pool: "default", VCPUs: 0, MemoryMB: 1024},
		},
		{
			name: "vcpus too large",
			req:  vmCreateRequest{Name: "x", Template: "ubuntu", Pool: "default", VCPUs: 129, MemoryMB: 1024},
		},
		{
			name: "memory too small",
			req:  vmCreateRequest{Name: "x", Template: "ubuntu", Pool: "default", VCPUs: 2, MemoryMB: 64},
		},
		{
			name: "memory too large",
			req:  vmCreateRequest{Name: "x", Template: "ubuntu", Pool: "default", VCPUs: 2, MemoryMB: 1 << 30},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &captureWriter{}
			got := validateCreateRequest(rec, fakeRequest(), tc.req)
			if got != tc.ok {
				t.Errorf("validateCreateRequest(%s) = %v, want %v", tc.name, got, tc.ok)
			}
		})
	}
}

// TestBuildNoEligibleDetails verifies the 409 envelope's
// `filtered_due_to_pressure` payload: it renders each nullable pressure
// timestamp only when its condition is active, and surfaces the `pool`
// name on pool-scoped entries. The nil-safety guard on the three
// `*time.Time` fields prevents `.IsZero()` from being called on a nil
// pointer when only system_disk_pressure or disk_pressure is active.
func TestBuildNoEligibleDetails(t *testing.T) {
	t.Parallel()

	mem := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	sys := time.Date(2026, 5, 12, 12, 5, 0, 0, time.UTC)
	pool := time.Date(2026, 5, 12, 12, 10, 0, 0, time.UTC)

	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

	cases := []struct {
		name string
		err  error
		want map[string]any
	}{
		{
			name: "nil error returns nil",
			err:  nil,
			want: nil,
		},
		{
			name: "bare ErrNoEligibleNodes returns nil",
			err:  scheduler.ErrNoEligibleNodes,
			want: nil,
		},
		{
			name: "wrapped ErrNoEligibleNodes without pressure detail returns nil",
			err:  fmt.Errorf("upstream: %w", scheduler.ErrNoEligibleNodes),
			want: nil,
		},
		{
			name: "empty pressure detail returns nil",
			err:  scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{Nodes: nil}),
			want: nil,
		},
		{
			name: "memory pressure only",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:                "node-a",
					Conditions:          []string{"memory_pressure"},
					MemoryPressureSince: &mem,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                  "node-a",
					"conditions":            []string{"memory_pressure"},
					"memory_pressure_since": rfc(mem),
				}},
			},
		},
		{
			name: "system_disk pressure only (A3 regression)",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:                    "node-a",
					Conditions:              []string{"system_disk_pressure"},
					SystemDiskPressureSince: &sys,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                       "node-a",
					"conditions":                 []string{"system_disk_pressure"},
					"system_disk_pressure_since": rfc(sys),
				}},
			},
		},
		{
			name: "pool disk pressure only (A3 regression)",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:              "node-a",
					Pool:              "pool-mvp",
					Conditions:        []string{"disk_pressure"},
					DiskPressureSince: &pool,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                "node-a",
					"pool":                "pool-mvp",
					"conditions":          []string{"disk_pressure"},
					"disk_pressure_since": rfc(pool),
				}},
			},
		},
		{
			name: "combined memory + system_disk on same node",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:                    "node-a",
					Conditions:              []string{"memory_pressure", "system_disk_pressure"},
					MemoryPressureSince:     &mem,
					SystemDiskPressureSince: &sys,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                       "node-a",
					"conditions":                 []string{"memory_pressure", "system_disk_pressure"},
					"memory_pressure_since":      rfc(mem),
					"system_disk_pressure_since": rfc(sys),
				}},
			},
		},
		{
			name: "combined non-memory: node system_disk + pool disk (A3 regression)",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{
					{
						Node:                    "node-a",
						Conditions:              []string{"system_disk_pressure"},
						SystemDiskPressureSince: &sys,
					},
					{
						Node:              "node-a",
						Pool:              "pool-mvp",
						Conditions:        []string{"disk_pressure"},
						DiskPressureSince: &pool,
					},
				},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{
					{
						"node":                       "node-a",
						"conditions":                 []string{"system_disk_pressure"},
						"system_disk_pressure_since": rfc(sys),
					},
					{
						"node":                "node-a",
						"pool":                "pool-mvp",
						"conditions":          []string{"disk_pressure"},
						"disk_pressure_since": rfc(pool),
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildNoEligibleDetails(tc.err)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("buildNoEligibleDetails mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBuildNoEligibleDetails_DoesNotPanicOnNilTimestamps is the
// targeted A3 regression net. The pre-fix implementation called
// `.IsZero()` on a nil *time.Time pointer; any pressure type other
// than memory (where MemoryPressureSince was always populated)
// triggered the nil-deref. Asserting "does not panic" in isolation
// catches future drift if a new pressure timestamp is added without
// matching nil-check.
func TestBuildNoEligibleDetails_DoesNotPanicOnNilTimestamps(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildNoEligibleDetails panicked: %v", r)
		}
	}()

	err := scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
		Nodes: []scheduler.PressuredNode{{
			Node:       "node-a",
			Conditions: []string{"system_disk_pressure"},
			// All three *time.Time fields intentionally nil — exercises
			// the pre-fix panic site.
		}},
	})

	got := buildNoEligibleDetails(err)
	if got == nil {
		t.Fatalf("buildNoEligibleDetails returned nil; want envelope")
	}
	filtered, ok := got["filtered_due_to_pressure"].([]map[string]any)
	if !ok || len(filtered) != 1 {
		t.Fatalf("filtered_due_to_pressure shape mismatch: %v", got["filtered_due_to_pressure"])
	}
	// None of the *_pressure_since keys should be present when the
	// timestamp pointer was nil.
	for _, key := range []string{"memory_pressure_since", "system_disk_pressure_since", "disk_pressure_since"} {
		if _, ok := filtered[0][key]; ok {
			t.Errorf("filtered_due_to_pressure[0] unexpectedly contains %q with nil source", key)
		}
	}
	// And ensure the wrapped ErrNoEligibleNodes sentinel is still
	// detectable through the test helper's error chain (defensive — if
	// the helper's wrap pattern changes the extractor breaks silently).
	if !errors.Is(err, scheduler.ErrNoEligibleNodes) {
		t.Errorf("test helper error chain does not unwrap to ErrNoEligibleNodes")
	}
}
