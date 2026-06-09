// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// overlayDhcpNet returns an egress=nat overlay with DHCP enabled, a subnet, and
// one MAC->IP reservation - the only shape for which DHCP registration fires.
func overlayDhcpNet() heartbeat.DeclaredNetwork {
	d := overlayEgressNet()
	subnet := "10.62.0.0/24"
	d.Subnet = &subnet
	d.DhcpEnabled = true
	d.Reservations = []heartbeat.DhcpReservation{
		{MAC: "52:54:00:aa:bb:cc", IP: "10.62.0.5"},
	}
	return d
}

func TestApplyOverlayEgressRegistersDHCP(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayDhcpNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(fake.RegisterCalls) != 1 {
		t.Fatalf("RegisterCalls = %d, want 1", len(fake.RegisterCalls))
	}
	cfg := fake.RegisterCalls[0]
	if cfg.NetworkID != "ov1" {
		t.Errorf("NetworkID = %q, want ov1", cfg.NetworkID)
	}
	if cfg.Bridge != "otb1000" {
		t.Errorf("Bridge = %q, want otb1000", cfg.Bridge)
	}
	if cfg.Subnet.String() != "10.62.0.0/24" {
		t.Errorf("Subnet = %v, want 10.62.0.0/24", cfg.Subnet)
	}
	if len(cfg.Reservations) != 1 {
		t.Fatalf("Reservations = %d, want 1", len(cfg.Reservations))
	}
	res := cfg.Reservations[0]
	if res.MAC.String() != "52:54:00:aa:bb:cc" {
		t.Errorf("reservation MAC = %q, want 52:54:00:aa:bb:cc", res.MAC.String())
	}
	if res.IP.String() != "10.62.0.5" {
		t.Errorf("reservation IP = %q, want 10.62.0.5", res.IP.String())
	}
	if !rec.applied["ov1"].HasDHCP {
		t.Errorf("applied[ov1].HasDHCP = false, want true")
	}
}

func TestApplyOverlayDHCPRegisterErrorStaysReady(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{Errs: map[string]error{"RegisterNetwork": errors.New("boom")}}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayDhcpNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	// A DHCP register failure must NOT fail the network - that would wrongly
	// de-schedule the node despite a converged datapath.
	if rep.ReconciliationStatus != "ready" {
		t.Errorf("status = %q, want ready (dhcp register failure must not fail the network)", rep.ReconciliationStatus)
	}
}

func TestApplyOverlayNoDHCPSkipsRegister(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayEgressNet()}, // DhcpEnabled false
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(fake.RegisterCalls) != 0 {
		t.Errorf("RegisterCalls = %d, want 0 for a non-dhcp overlay", len(fake.RegisterCalls))
	}
	if rec.applied["ov1"].HasDHCP {
		t.Errorf("HasDHCP set for a non-dhcp overlay")
	}
}

func TestApplyOverlayDHCPTeardownDeregisters(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayDhcpNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background()) // materialise
	// CP deletes the network: empty declared set.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	rec.reconcile(context.Background()) // teardown

	if len(fake.DeregisterCalls) != 1 || fake.DeregisterCalls[0] != "ov1" {
		t.Errorf("DeregisterCalls = %v, want [ov1]", fake.DeregisterCalls)
	}
}
