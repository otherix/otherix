// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
)

func TestLBBackendHealthUpsertListCascade(t *testing.T) {
	s, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	lbID, vmA, vmB := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.UpsertLBBackendHealth(ctx, lbID, vmA, true, now); err != nil {
		t.Fatalf("UpsertLBBackendHealth(A) error = %v", err)
	}
	if err := s.UpsertLBBackendHealth(ctx, lbID, vmB, false, now); err != nil {
		t.Fatalf("UpsertLBBackendHealth(B) error = %v", err)
	}
	got, err := s.ListLBBackendHealth(ctx, lbID)
	if err != nil {
		t.Fatalf("ListLBBackendHealth() error = %v", err)
	}
	if len(got) != 2 || !got[vmA].Healthy || got[vmB].Healthy {
		t.Errorf("ListLBBackendHealth() = %+v, want A healthy, B unhealthy", got)
	}
	if !got[vmA].ReportedAt.Equal(now) {
		t.Errorf("ReportedAt = %v, want %v", got[vmA].ReportedAt, now)
	}

	// A cross-lb record must not leak into this lb's list.
	otherLB := uuid.New()
	if err := s.UpsertLBBackendHealth(ctx, otherLB, vmA, false, now); err != nil {
		t.Fatalf("UpsertLBBackendHealth(other) error = %v", err)
	}
	if got, _ = s.ListLBBackendHealth(ctx, lbID); len(got) != 2 {
		t.Errorf("ListLBBackendHealth(lbID) leaked cross-lb rows: %+v", got)
	}
}

func TestDeleteLoadBalancerCascadesHealth(t *testing.T) {
	s, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	lb, err := s.CreateLoadBalancer(ctx, lbParams(uniqueLBName("lb-health-cascade"), owner))
	if err != nil {
		t.Fatalf("CreateLoadBalancer() error = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	vmA, vmB := uuid.New(), uuid.New()
	if err := s.UpsertLBBackendHealth(ctx, lb.ID, vmA, true, now); err != nil {
		t.Fatalf("UpsertLBBackendHealth(A) error = %v", err)
	}
	if err := s.UpsertLBBackendHealth(ctx, lb.ID, vmB, false, now); err != nil {
		t.Fatalf("UpsertLBBackendHealth(B) error = %v", err)
	}

	if err := s.DeleteLoadBalancer(ctx, lb.ID); err != nil {
		t.Fatalf("DeleteLoadBalancer() error = %v", err)
	}

	got, err := s.ListLBBackendHealth(ctx, lb.ID)
	if err != nil {
		t.Fatalf("ListLBBackendHealth() after delete error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListLBBackendHealth() after delete = %+v, want empty map", got)
	}
}
