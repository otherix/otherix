// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// countSubstr returns how many lines in s contain sub.
func countSubstr(s, sub string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

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
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
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
	if cfg.Bridge != "otvb1000" {
		t.Errorf("Bridge = %q, want otvb1000", cfg.Bridge)
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

// TestApplyOverlayDNSWithoutEgressRegistersAndPlumbsL3 drives a dns=true,
// dhcp=true, egress=none overlay through the real applyOverlay entry and asserts
// that DHCP registers advertising DNS but not a default route, that the L3
// anycast gateway is installed (so the host-side DNS forwarder's replies reach
// the VM), and that masquerade is NOT (no NAT without egress).
func TestApplyOverlayDNSWithoutEgressRegistersAndPlumbsL3(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := overlayDhcpNet() // dhcp + subnet + reservation
	d.Egress = ""         // no NAT egress; DNS-only L3
	d.DNSEnabled = true
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{d},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(fake.RegisterCalls) != 1 {
		t.Fatalf("RegisterCalls = %d, want 1 (dhcp must register without egress)", len(fake.RegisterCalls))
	}
	cfg := fake.RegisterCalls[0]
	if !cfg.AdvertiseDNS {
		t.Errorf("AdvertiseDNS = false, want true (dns enabled)")
	}
	if cfg.AdvertiseDefaultRoute {
		t.Errorf("AdvertiseDefaultRoute = true, want false (no egress)")
	}
	// L3 services: anycast gateway must be installed so the DNS forwarder's
	// replies are routed back to the VM.
	if len(f.AnycastGatewayCalls) != 1 || f.AnycastGatewayCalls[0].Bridge != "otvb1000" {
		t.Errorf("AnycastGatewayCalls = %+v, want one for otvb1000", f.AnycastGatewayCalls)
	}
	// No NAT without egress.
	if len(f.MasqueradeIfaceCalls) != 0 {
		t.Errorf("MasqueradeIfaceCalls = %+v, want none (no egress)", f.MasqueradeIfaceCalls)
	}
	if rec.applied["ov1"].HasEgress {
		t.Errorf("HasEgress = true, want false (no egress)")
	}
}

// TestApplyOverlayNatDNSDisabledWithholdsResolver asserts that an explicit
// dns=false is authoritative even under egress=nat: the DHCP responder does NOT
// advertise the resolver (no option 6), while egress is unaffected - the default
// route is still advertised. dns and egress are independent.
func TestApplyOverlayNatDNSDisabledWithholdsResolver(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := overlayDhcpNet()
	d.Egress = "nat"
	d.DNSEnabled = false
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{d},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(fake.RegisterCalls) != 1 {
		t.Fatalf("RegisterCalls = %d, want 1", len(fake.RegisterCalls))
	}
	cfg := fake.RegisterCalls[0]
	if cfg.AdvertiseDNS {
		t.Errorf("AdvertiseDNS = true, want false (explicit dns=false withholds the resolver even under nat)")
	}
	if !cfg.AdvertiseDefaultRoute {
		t.Errorf("AdvertiseDefaultRoute = false, want true (nat advertises the default route independent of dns)")
	}
}

func TestApplyOverlayDHCPRegisterErrorStaysReady(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{Errs: map[string]error{"RegisterNetwork": errors.New("boom")}}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
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

// TestDHCPRegisterFailureLogsOncePerError asserts a permanent DHCP-register
// failure (e.g. missing CAP_NET_RAW) logs the WARN exactly once across many
// reconcile passes, not once per pass - registration is re-asserted every pass
// to converge reservations, so a per-pass WARN would spam the log forever.
func TestDHCPRegisterFailureLogsOncePerError(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{Errs: map[string]error{"RegisterNetwork": errors.New("boom")}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec, err := NewNetworks(f, fake, log, time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayDhcpNet()},
		SelfOverlayIP:    &ip,
	})

	const passes = 5
	for range passes {
		rec.reconcile(context.Background())
	}
	if len(fake.RegisterCalls) != passes {
		t.Fatalf("RegisterCalls = %d, want %d (register re-asserted every pass)", len(fake.RegisterCalls), passes)
	}

	if got := countSubstr(buf.String(), "overlay dhcp: register failed"); got != 1 {
		t.Errorf("register-failed WARN count = %d across %d passes, want 1", got, passes)
	}
}

// TestDHCPRegisterRecoveryRelogsOnReFailure asserts a recovered registration
// emits a single INFO and clears the dedup state, so a later re-failure logs
// the WARN again.
func TestDHCPRegisterRecoveryRelogsOnReFailure(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{Errs: map[string]error{"RegisterNetwork": errors.New("boom")}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec, err := NewNetworks(f, fake, log, time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayDhcpNet()},
		SelfOverlayIP:    &ip,
	})

	rec.reconcile(context.Background()) // fail -> 1 WARN
	delete(fake.Errs, "RegisterNetwork")
	rec.reconcile(context.Background()) // success -> 1 recovered INFO, clears state
	fake.Errs = map[string]error{"RegisterNetwork": errors.New("boom")}
	rec.reconcile(context.Background()) // fail again -> WARN re-logs

	out := buf.String()
	if got := countSubstr(out, "overlay dhcp: register failed"); got != 2 {
		t.Errorf("register-failed WARN count = %d, want 2 (re-fail must re-log)", got)
	}
	if got := countSubstr(out, "overlay dhcp: register recovered"); got != 1 {
		t.Errorf("register-recovered INFO count = %d, want 1", got)
	}
}

func TestApplyOverlayNoDHCPSkipsRegister(t *testing.T) {
	f := readyEgressFabric()
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
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
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
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
