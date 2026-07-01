// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"fmt"
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
