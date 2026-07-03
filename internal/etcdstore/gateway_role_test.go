// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

func TestCreateNodePersistsGatewayRole(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	gwName := uniqueNodeName("gw")
	gwp := nodeParams(gwName)
	gwp.Gateway = true
	if _, err := s.CreateNode(ctx, gwp); err != nil {
		t.Fatalf("CreateNode(gateway): %v", err)
	}
	gw, err := s.NodeByName(ctx, gwName)
	if err != nil {
		t.Fatalf("NodeByName(gateway): %v", err)
	}
	if !gw.HasRole(store.NodeRoleGateway) {
		t.Errorf("gateway node GatewayRole = %v, want true", gw.GatewayRole)
	}

	hvName := uniqueNodeName("hv")
	if _, err := s.CreateNode(ctx, nodeParams(hvName)); err != nil {
		t.Fatalf("CreateNode(default): %v", err)
	}
	hv, err := s.NodeByName(ctx, hvName)
	if err != nil {
		t.Fatalf("NodeByName(default): %v", err)
	}
	if hv.HasRole(store.NodeRoleGateway) {
		t.Errorf("default node GatewayRole = %v, want false", hv.GatewayRole)
	}

	eff, err := s.NodeEffectiveByID(ctx, gwp.ID)
	if err != nil {
		t.Fatalf("NodeEffectiveByID(gateway): %v", err)
	}
	if !eff.GatewayRole {
		t.Errorf("effective gateway row GatewayRole = false, want true")
	}
}
