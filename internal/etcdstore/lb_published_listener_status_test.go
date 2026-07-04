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

// TestDeleteNodeReapsPublishedListenerStatus proves node delete reaps every
// published-listener status row keyed on the deleted node across all LBs, while
// leaving another node's rows and the LB rows themselves intact.
func TestDeleteNodeReapsPublishedListenerStatus(t *testing.T) {
	s, _ := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)

	// Two published LBs, each carrying a listener-status row for node N and M.
	pub1, pub2 := int32(30080), int32(30081)
	lbp1 := lbParams(uniqueLBName("reap-lb-1"), owner)
	lbp1.PublishedPort = &pub1
	lb1, err := s.CreateLoadBalancer(ctx, lbp1)
	if err != nil {
		t.Fatalf("CreateLoadBalancer(1) error = %v", err)
	}
	lbp2 := lbParams(uniqueLBName("reap-lb-2"), owner)
	lbp2.PublishedPort = &pub2
	lb2, err := s.CreateLoadBalancer(ctx, lbp2)
	if err != nil {
		t.Fatalf("CreateLoadBalancer(2) error = %v", err)
	}

	nodeN := nodeParams(uniqueNodeName("reap-n"))
	if _, err := s.CreateNode(ctx, nodeN); err != nil {
		t.Fatalf("CreateNode(N) error = %v", err)
	}
	nodeM := nodeParams(uniqueNodeName("reap-m"))
	if _, err := s.CreateNode(ctx, nodeM); err != nil {
		t.Fatalf("CreateNode(M) error = %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	for _, lbID := range []uuid.UUID{lb1.ID, lb2.ID} {
		if err := s.UpsertLBPublishedListenerStatus(ctx, lbID, nodeN.ID, 30080, true, "", now); err != nil {
			t.Fatalf("UpsertLBPublishedListenerStatus(N) error = %v", err)
		}
		if err := s.UpsertLBPublishedListenerStatus(ctx, lbID, nodeM.ID, 30080, true, "", now); err != nil {
			t.Fatalf("UpsertLBPublishedListenerStatus(M) error = %v", err)
		}
	}

	if _, err := s.DeleteNode(ctx, nodeN.ID, true, uuid.New()); err != nil {
		t.Fatalf("DeleteNode(N) error = %v", err)
	}

	for _, lbID := range []uuid.UUID{lb1.ID, lb2.ID} {
		got, err := s.ListLBPublishedListenerStatus(ctx, lbID)
		if err != nil {
			t.Fatalf("ListLBPublishedListenerStatus(%v) error = %v", lbID, err)
		}
		if len(got) != 1 {
			t.Fatalf("ListLBPublishedListenerStatus(%v) len = %d, want 1 (N reaped, M intact): %+v", lbID, len(got), got)
		}
		if got[0].NodeID != nodeM.ID {
			t.Errorf("surviving row NodeID = %v, want M %v (N should be reaped)", got[0].NodeID, nodeM.ID)
		}
		// The LB row itself must remain readable.
		if _, err := s.LoadBalancerByID(ctx, lbID); err != nil {
			t.Errorf("LoadBalancerByID(%v) after node delete error = %v, want the LB intact", lbID, err)
		}
	}
}
