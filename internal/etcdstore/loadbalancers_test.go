// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

func seedLBOwner(t *testing.T, s *etcdstore.Store) uuid.UUID {
	t.Helper()
	u, err := s.CreateUser(context.Background(), userParams(uniqueEmail("lb-owner")))
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	return u.ID
}

func lbParams(name string, owner uuid.UUID) store.CreateLoadBalancerParams {
	return store.CreateLoadBalancerParams{
		ID:       uuid.New(),
		Name:     name,
		OwnerID:  owner,
		Port:     8080,
		Selector: map[string]string{"app": "web"},
	}
}

func uniqueLBName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func TestLoadBalancerCreateAndGet(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	p := lbParams(uniqueLBName("lb"), owner)
	created, err := s.CreateLoadBalancer(ctx, p)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}
	if created.ID != p.ID || created.Name != p.Name || created.OwnerID != owner ||
		created.Port != p.Port || created.CreatedAt.IsZero() {
		t.Errorf("CreateLoadBalancer = %+v, want id/name/owner/port set + created_at stamped", created)
	}
	if diff := cmp.Diff(p.Selector, created.Selector); diff != "" {
		t.Errorf("Selector round-trip mismatch (-want +got):\n%s", diff)
	}

	byID, err := s.LoadBalancerByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadBalancerByID: %v", err)
	}
	if byID.ID != p.ID || byID.Name != p.Name {
		t.Errorf("LoadBalancerByID = %+v, want id=%v name=%q", byID, p.ID, p.Name)
	}
	if diff := cmp.Diff(p.Selector, byID.Selector); diff != "" {
		t.Errorf("LoadBalancerByID Selector mismatch (-want +got):\n%s", diff)
	}

	byName, err := s.LoadBalancerByName(ctx, strings.ToUpper(p.Name))
	if err != nil {
		t.Fatalf("LoadBalancerByName(upper): %v", err)
	}
	if byName.ID != p.ID {
		t.Errorf("LoadBalancerByName = %v, want id %v", byName.ID, p.ID)
	}
}

func TestLoadBalancerByIDNotFound(t *testing.T) {
	s, _ := startStore(t)
	if _, err := s.LoadBalancerByID(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadBalancerByID(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestLoadBalancerByNameNotFound(t *testing.T) {
	s, _ := startStore(t)
	if _, err := s.LoadBalancerByName(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadBalancerByName(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestCreateLoadBalancerDuplicateNameCaseInsensitive(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)
	name := uniqueLBName("dup")

	if _, err := s.CreateLoadBalancer(ctx, lbParams(name, owner)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	clash := lbParams(strings.ToUpper(name), owner)
	clash.Port = 80
	if _, err := s.CreateLoadBalancer(ctx, clash); !errors.Is(err, store.ErrLoadBalancerNameExists) {
		t.Fatalf("dup create err = %v, want ErrLoadBalancerNameExists", err)
	}
}

func TestLoadBalancerUpdateRenameFreeName(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	p := lbParams(uniqueLBName("upd"), owner)
	created, err := s.CreateLoadBalancer(ctx, p)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}
	newName := uniqueLBName("upd-renamed")
	updated, err := s.UpdateLoadBalancer(ctx, store.UpdateLoadBalancerParams{
		ID: p.ID, Name: newName, Port: 9090, Selector: map[string]string{"app": "api"},
	})
	if err != nil {
		t.Fatalf("UpdateLoadBalancer: %v", err)
	}
	if updated.Name != newName || updated.Port != 9090 {
		t.Errorf("UpdateLoadBalancer = %+v, want renamed/9090", updated)
	}
	if diff := cmp.Diff(map[string]string{"app": "api"}, updated.Selector); diff != "" {
		t.Errorf("updated Selector mismatch (-want +got):\n%s", diff)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("updated_at not bumped: created %v updated %v", created.UpdatedAt, updated.UpdatedAt)
	}
	// Old name free, new name now taken.
	if _, err := s.CreateLoadBalancer(ctx, lbParams(p.Name, owner)); err != nil {
		t.Errorf("old name not reusable after rename: %v", err)
	}
	if _, err := s.CreateLoadBalancer(ctx, lbParams(newName, owner)); !errors.Is(err, store.ErrLoadBalancerNameExists) {
		t.Errorf("new name should be taken, got %v", err)
	}
}

func TestLoadBalancerUpdateRenameCollision(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	taken := uniqueLBName("taken")
	if _, err := s.CreateLoadBalancer(ctx, lbParams(taken, owner)); err != nil {
		t.Fatalf("seed taken: %v", err)
	}
	mover := lbParams(uniqueLBName("mover"), owner)
	if _, err := s.CreateLoadBalancer(ctx, mover); err != nil {
		t.Fatalf("seed mover: %v", err)
	}
	_, err := s.UpdateLoadBalancer(ctx, store.UpdateLoadBalancerParams{
		ID: mover.ID, Name: taken, Port: 8080, Selector: map[string]string{"a": "b"},
	})
	if !errors.Is(err, store.ErrLoadBalancerNameExists) {
		t.Errorf("rename collision err = %v, want ErrLoadBalancerNameExists", err)
	}
}

func TestLoadBalancerUpdateNotFound(t *testing.T) {
	s, _ := startStore(t)
	_, err := s.UpdateLoadBalancer(context.Background(), store.UpdateLoadBalancerParams{
		ID: uuid.New(), Name: "x", Port: 80,
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateLoadBalancer(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestLoadBalancerListOrderingPaginationAndDeletedExcluded(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	ids := make([]uuid.UUID, 0, 3)
	for i := 0; i < 3; i++ {
		p := lbParams(uniqueLBName(fmt.Sprintf("list%d", i)), owner)
		if _, err := s.CreateLoadBalancer(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, p.ID)
		time.Sleep(2 * time.Millisecond) // keep created_at strictly increasing
	}

	all, err := s.ListLoadBalancers(ctx, store.ListLoadBalancersParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListLoadBalancers all: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("ListLoadBalancers returned %d, want >= 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
			t.Errorf("list not ascending at %d: %v before %v", i, all[i].CreatedAt, all[i-1].CreatedAt)
		}
	}

	first := all[0]
	page, err := s.ListLoadBalancers(ctx, store.ListLoadBalancersParams{
		CursorCreatedAt: &first.CreatedAt, CursorID: &first.ID, LimitCount: 1,
	})
	if err != nil {
		t.Fatalf("ListLoadBalancers paged: %v", err)
	}
	if len(page) != 1 || page[0].ID != all[1].ID {
		t.Errorf("paged after first = %v, want [%v]", page, all[1].ID)
	}

	if err := s.DeleteLoadBalancer(ctx, ids[0]); err != nil {
		t.Fatalf("DeleteLoadBalancer: %v", err)
	}
	after, err := s.ListLoadBalancers(ctx, store.ListLoadBalancersParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListLoadBalancers after delete: %v", err)
	}
	for _, lb := range after {
		if lb.ID == ids[0] {
			t.Errorf("deleted load balancer %v still listed", ids[0])
		}
	}
}

func TestLoadBalancerDeleteAndNameReuse(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)
	name := uniqueLBName("del")

	lb, err := s.CreateLoadBalancer(ctx, lbParams(name, owner))
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}
	if err := s.DeleteLoadBalancer(ctx, lb.ID); err != nil {
		t.Fatalf("DeleteLoadBalancer: %v", err)
	}
	if _, err := s.LoadBalancerByID(ctx, lb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadBalancerByID after delete = %v, want store.ErrNotFound", err)
	}
	if _, err := s.LoadBalancerByName(ctx, name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LoadBalancerByName after delete = %v, want store.ErrNotFound", err)
	}
	// Name reusable after soft-delete (guard dropped).
	if _, err := s.CreateLoadBalancer(ctx, lbParams(name, owner)); err != nil {
		t.Errorf("name not reusable after delete: %v", err)
	}
}

func TestLoadBalancerDeleteNotFound(t *testing.T) {
	s, _ := startStore(t)
	if err := s.DeleteLoadBalancer(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteLoadBalancer(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestListVMsByOwnerFilters(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	ownerA := uuid.New()
	ownerB := uuid.New()

	// Two live VMs for ownerA, one soft-deleted for ownerA, one live for ownerB.
	now := time.Now().UTC()
	deleted := now
	seed := func(owner uuid.UUID, deletedAt *time.Time) uuid.UUID {
		id := uuid.New()
		vm := store.VM{ID: id, OwnerID: owner, Name: uniqueLBName("vm"), CreatedAt: now, UpdatedAt: now, DeletedAt: deletedAt}
		if err := cli.PutJSON(ctx, etcd.Key("vms", id.String()), vm); err != nil {
			t.Fatalf("seed vm: %v", err)
		}
		return id
	}
	a1 := seed(ownerA, nil)
	a2 := seed(ownerA, nil)
	seed(ownerA, &deleted) // soft-deleted, must be excluded
	seed(ownerB, nil)      // other owner, must be excluded

	got, err := s.ListVMsByOwner(ctx, ownerA)
	if err != nil {
		t.Fatalf("ListVMsByOwner: %v", err)
	}
	want := map[uuid.UUID]bool{a1: true, a2: true}
	if len(got) != len(want) {
		t.Fatalf("ListVMsByOwner len = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, vm := range got {
		if !want[vm.ID] {
			t.Errorf("ListVMsByOwner returned unexpected vm %v (owner %v, deleted %v)", vm.ID, vm.OwnerID, vm.DeletedAt)
		}
	}
}

// TestLoadBalancerPublishedPortRoundTrip verifies the three publish fields
// survive a create/get, and that a duplicate published_port across two LBs is
// rejected with ErrLoadBalancerPublishedPortExists.
func TestLoadBalancerPublishedPortRoundTrip(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	port := int32(8080)
	p := lbParams(uniqueLBName("lb"), owner)
	p.PublishedPort = &port
	p.Protocol = "tcp"
	p.SourceCIDRs = []string{"203.0.113.0/24"}

	created, err := s.CreateLoadBalancer(ctx, p)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}
	if created.PublishedPort == nil || *created.PublishedPort != port {
		t.Errorf("PublishedPort = %v, want %d", created.PublishedPort, port)
	}
	if created.Protocol != "tcp" {
		t.Errorf("Protocol = %q, want tcp", created.Protocol)
	}
	if diff := cmp.Diff([]string{"203.0.113.0/24"}, created.SourceCIDRs); diff != "" {
		t.Errorf("SourceCIDRs mismatch (-want +got):\n%s", diff)
	}

	// Second LB claiming the same published_port must be rejected.
	clash := lbParams(uniqueLBName("lb"), owner)
	clash.PublishedPort = &port
	if _, err := s.CreateLoadBalancer(ctx, clash); !errors.Is(err, store.ErrLoadBalancerPublishedPortExists) {
		t.Fatalf("dup published_port err = %v, want ErrLoadBalancerPublishedPortExists", err)
	}
}

// TestLoadBalancerPublishedPortSwapAndRelease verifies PATCH can change,
// clear, and re-claim a published_port, freeing the guard each time.
func TestLoadBalancerPublishedPortSwapAndRelease(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	p1, p2 := int32(9001), int32(9002)
	base := lbParams(uniqueLBName("lb"), owner)
	base.PublishedPort = &p1
	base.Protocol = "tcp"
	lb, err := s.CreateLoadBalancer(ctx, base)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Change p1 -> p2, then a new LB may claim the freed p1.
	upd := store.UpdateLoadBalancerParams{
		ID: lb.ID, Name: lb.Name, Port: lb.Port, Selector: lb.Selector,
		HealthCheck: lb.HealthCheck, PublishedPort: &p2, Protocol: "tcp",
	}
	if _, err := s.UpdateLoadBalancer(ctx, upd); err != nil {
		t.Fatalf("update swap: %v", err)
	}
	reuser := lbParams(uniqueLBName("lb"), owner)
	reuser.PublishedPort = &p1
	if _, err := s.CreateLoadBalancer(ctx, reuser); err != nil {
		t.Fatalf("reuse freed p1: %v", err)
	}

	// Clear p2 (unpublish); a later LB may claim p2.
	clear := upd
	clear.PublishedPort = nil
	clear.Protocol = ""
	if _, err := s.UpdateLoadBalancer(ctx, clear); err != nil {
		t.Fatalf("update clear: %v", err)
	}
	reuser2 := lbParams(uniqueLBName("lb"), owner)
	reuser2.PublishedPort = &p2
	if _, err := s.CreateLoadBalancer(ctx, reuser2); err != nil {
		t.Fatalf("reuse freed p2: %v", err)
	}
}

// lbPublishedPortGuardKey mirrors the store's unexported published-port guard
// key so a test can seed or read the guard directly from the external package.
func lbPublishedPortGuardKey(port int32) string {
	return etcd.Key("uniq", "lb_published_port", strconv.Itoa(int(port)))
}

// TestLoadBalancerReclaimOwnLeakedPortGuard verifies that a load balancer can
// re-claim a published-port guard that already points at itself (a leaked
// guard whose row no longer references the port), while a guard held by a
// DIFFERENT load balancer still rejects the claim.
func TestLoadBalancerReclaimOwnLeakedPortGuard(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	port := int32(7100)
	lb, err := s.CreateLoadBalancer(ctx, lbParams(uniqueLBName("lb"), owner))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate a leaked guard: the port guard points at lb, but lb's row does
	// not reference the port (lb is unpublished). Write the guard directly.
	if _, err := cli.Raw().Put(ctx, lbPublishedPortGuardKey(port), lb.ID.String()); err != nil {
		t.Fatalf("seed leaked guard: %v", err)
	}

	// Publishing lb onto its own leaked port must succeed (reclaim).
	upd := store.UpdateLoadBalancerParams{
		ID: lb.ID, Name: lb.Name, Port: lb.Port, Selector: lb.Selector,
		HealthCheck: lb.HealthCheck, PublishedPort: &port, Protocol: "tcp",
	}
	if _, err := s.UpdateLoadBalancer(ctx, upd); err != nil {
		t.Fatalf("reclaim own leaked port: %v", err)
	}

	// A different LB claiming a port whose guard is held by lb must be rejected.
	other, err := s.CreateLoadBalancer(ctx, lbParams(uniqueLBName("lb"), owner))
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	otherPort := int32(7101)
	if _, err := cli.Raw().Put(ctx, lbPublishedPortGuardKey(otherPort), lb.ID.String()); err != nil {
		t.Fatalf("seed other-owned guard: %v", err)
	}
	updOther := store.UpdateLoadBalancerParams{
		ID: other.ID, Name: other.Name, Port: other.Port, Selector: other.Selector,
		HealthCheck: other.HealthCheck, PublishedPort: &otherPort, Protocol: "tcp",
	}
	if _, err := s.UpdateLoadBalancer(ctx, updOther); !errors.Is(err, store.ErrLoadBalancerPublishedPortExists) {
		t.Fatalf("claim other-owned port err = %v, want ErrLoadBalancerPublishedPortExists", err)
	}
}
