// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestCreateLoadBalancerPersistsHealthCheck(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	owner := seedLBOwner(t, s)

	hc := store.LoadBalancerHealthCheck{
		Port: 8080, IntervalSeconds: 5, TimeoutSeconds: 1,
		HealthyThreshold: 2, UnhealthyThreshold: 3,
	}
	name := uniqueLBName("lb-hc")
	got, err := s.CreateLoadBalancer(ctx, store.CreateLoadBalancerParams{
		ID: uuid.New(), Name: name, OwnerID: owner, Port: 80,
		Selector: map[string]string{"app": "web"}, HealthCheck: hc,
	})
	if err != nil {
		t.Fatalf("CreateLoadBalancer() error = %v", err)
	}
	if got.HealthCheck != hc {
		t.Errorf("CreateLoadBalancer() HealthCheck = %+v, want %+v", got.HealthCheck, hc)
	}
	round, err := s.LoadBalancerByName(ctx, name)
	if err != nil {
		t.Fatalf("LoadBalancerByName() error = %v", err)
	}
	if round.HealthCheck != hc {
		t.Errorf("LoadBalancerByName() HealthCheck = %+v, want %+v", round.HealthCheck, hc)
	}
}
