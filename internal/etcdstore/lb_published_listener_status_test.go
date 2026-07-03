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

func TestLBPublishedListenerStatusUpsertListCascade(t *testing.T) {
	s, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	lbID, nodeA, nodeB := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.UpsertLBPublishedListenerStatus(ctx, lbID, nodeA, 8080, true, "", now); err != nil {
		t.Fatalf("UpsertLBPublishedListenerStatus(A) error = %v", err)
	}
	if err := s.UpsertLBPublishedListenerStatus(ctx, lbID, nodeB, 8080, false, "bind: address already in use", now); err != nil {
		t.Fatalf("UpsertLBPublishedListenerStatus(B) error = %v", err)
	}

	got, err := s.ListLBPublishedListenerStatus(ctx, lbID)
	if err != nil {
		t.Fatalf("ListLBPublishedListenerStatus() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListLBPublishedListenerStatus() len = %d, want 2", len(got))
	}
	for _, st := range got {
		switch st.NodeID {
		case nodeA:
			if !st.Bound || st.Error != "" || st.Port != 8080 || !st.ReportedAt.Equal(now) {
				t.Errorf("nodeA status = %+v, want bound port 8080 no error at %v", st, now)
			}
		case nodeB:
			if st.Bound || st.Error == "" || st.Port != 8080 {
				t.Errorf("nodeB status = %+v, want unbound port 8080 with error", st)
			}
		default:
			t.Errorf("unexpected NodeID %v in list", st.NodeID)
		}
	}

	// A cross-lb record must not leak into this lb's list.
	otherLB := uuid.New()
	if err := s.UpsertLBPublishedListenerStatus(ctx, otherLB, nodeA, 9090, true, "", now); err != nil {
		t.Fatalf("UpsertLBPublishedListenerStatus(other) error = %v", err)
	}
	if got, _ = s.ListLBPublishedListenerStatus(ctx, lbID); len(got) != 2 {
		t.Errorf("ListLBPublishedListenerStatus(lbID) leaked cross-lb rows: %+v", got)
	}
}

func TestDeleteLoadBalancerCascadesPublishedListenerStatus(t *testing.T) {
	s, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	lb, err := s.CreateLoadBalancer(ctx, lbParams(uniqueLBName("lb-listener-cascade"), owner))
	if err != nil {
		t.Fatalf("CreateLoadBalancer() error = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	nodeA, nodeB := uuid.New(), uuid.New()
	if err := s.UpsertLBPublishedListenerStatus(ctx, lb.ID, nodeA, 8080, true, "", now); err != nil {
		t.Fatalf("UpsertLBPublishedListenerStatus(A) error = %v", err)
	}
	if err := s.UpsertLBPublishedListenerStatus(ctx, lb.ID, nodeB, 8080, false, "boom", now); err != nil {
		t.Fatalf("UpsertLBPublishedListenerStatus(B) error = %v", err)
	}

	if err := s.DeleteLoadBalancer(ctx, lb.ID); err != nil {
		t.Fatalf("DeleteLoadBalancer() error = %v", err)
	}

	got, err := s.ListLBPublishedListenerStatus(ctx, lb.ID)
	if err != nil {
		t.Fatalf("ListLBPublishedListenerStatus() after delete error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListLBPublishedListenerStatus() after delete = %+v, want empty", got)
	}
}
