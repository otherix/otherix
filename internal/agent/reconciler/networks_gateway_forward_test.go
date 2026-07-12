// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// TestGatewayNodeEnablesIPForwardingWithoutNAT proves a gateway-role node turns
// on ip_forward on a reconcile pass even when it declares no egress=nat network.
// Without it a relay gateway has ip_forward=0 and silently blackholes A->G->B
// WireGuard relay transit.
func TestGatewayNodeEnablesIPForwardingWithoutNAT(t *testing.T) {
	f := &netfabric.FakeFabric{}
	// isGateway=true, and no networks declared at all (no nat path to trigger it).
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, false, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	rec.reconcile(context.Background())
	if f.EnableIPForwardingCalls == 0 {
		t.Errorf("gateway node did not enable ip_forward with no nat network; EnableIPForwardingCalls = 0")
	}
}

// TestNonGatewayNodeSkipsIPForwardingWithoutNAT is the revert-to-confirm: a plain
// node (isGateway=false) with no nat network must NOT touch ip_forward, so the
// gateway gate has teeth.
func TestNonGatewayNodeSkipsIPForwardingWithoutNAT(t *testing.T) {
	f := &netfabric.FakeFabric{}
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	rec.reconcile(context.Background())
	if f.EnableIPForwardingCalls != 0 {
		t.Errorf("non-gateway node enabled ip_forward with no nat network; EnableIPForwardingCalls = %d", f.EnableIPForwardingCalls)
	}
}

// TestGatewayForwardingFailureDoesNotCrash proves a failing EnableIPForwarding is
// best-effort: the reconcile pass still completes (retried next pass) and never
// panics or wedges the reconciler (fail toward inaction).
func TestGatewayForwardingFailureDoesNotCrash(t *testing.T) {
	f := &netfabric.FakeFabric{Errs: map[string]error{"EnableIPForwarding": errors.New("procfs boom")}}
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, false, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	rec.reconcile(context.Background()) // must not panic
	if f.EnableIPForwardingCalls != 1 {
		t.Errorf("EnableIPForwardingCalls = %d, want 1 (attempted once, best-effort)", f.EnableIPForwardingCalls)
	}
}
