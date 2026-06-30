// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/netfabric"
	avm "github.com/otherix/otherix/internal/agent/vm"
)

type fakeManager struct{ byName map[string]*avm.VM }

func (f *fakeManager) ByName(name string) (*avm.VM, error) {
	if v, ok := f.byName[name]; ok {
		return v, nil
	}
	return nil, avm.ErrNotFound
}

type fakeLeases map[string]netip.Addr

func (f fakeLeases) LookupByMAC(mac string) (netip.Addr, bool) { a, ok := f[mac]; return a, ok }

func TestResolveSSHTarget_RunningVMLeaseIPPort22(t *testing.T) {
	mgr := &fakeManager{byName: map[string]*avm.VM{
		"web01": {Name: "web01", Status: avm.StatusRunning, NICs: []netfabric.NIC{
			{MAC: "52:54:00:11:22:33"},
		}},
	}}
	leases := fakeLeases{"52:54:00:11:22:33": netip.MustParseAddr("10.42.0.7")}

	got, err := resolveSSHTarget(mgr, leases, "web01", 22)
	if err != nil {
		t.Fatalf("resolveSSHTarget(web01) error = %v", err)
	}
	if want := "10.42.0.7:22"; got != want {
		t.Errorf("resolveSSHTarget = %q, want %q", got, want)
	}
}

// TestResolveSSHTarget_ArbitraryPort proves the dialed target joins the
// lease-derived IP with the PASSED port (a non-22 port reaches e.g. psql), and
// that the host is still the agent's own lease, never anything caller-supplied:
// the port is the only new wire-influenced input, the IP stays lease-bound.
func TestResolveSSHTarget_ArbitraryPort(t *testing.T) {
	mgr := &fakeManager{byName: map[string]*avm.VM{
		"web01": {Name: "web01", Status: avm.StatusRunning, NICs: []netfabric.NIC{
			{MAC: "52:54:00:11:22:33"},
		}},
	}}
	leases := fakeLeases{"52:54:00:11:22:33": netip.MustParseAddr("10.42.0.7")}

	got, err := resolveSSHTarget(mgr, leases, "web01", 5432)
	if err != nil {
		t.Fatalf("resolveSSHTarget(web01, 5432) error = %v", err)
	}
	host, port, err := net.SplitHostPort(got)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", got, err)
	}
	if host != "10.42.0.7" {
		t.Errorf("resolveSSHTarget host = %q, want the lease IP %q", host, "10.42.0.7")
	}
	if port != "5432" {
		t.Errorf("resolveSSHTarget port = %q, want %q", port, "5432")
	}
}

func TestResolveSSHTarget_RejectsNonRunning(t *testing.T) {
	mgr := &fakeManager{byName: map[string]*avm.VM{
		"web01": {Name: "web01", Status: avm.StatusStopped, NICs: []netfabric.NIC{
			{MAC: "52:54:00:11:22:33"},
		}},
	}}
	leases := fakeLeases{"52:54:00:11:22:33": netip.MustParseAddr("10.42.0.7")}

	if _, err := resolveSSHTarget(mgr, leases, "web01", 22); err == nil {
		t.Errorf("resolveSSHTarget(stopped) = nil error, want rejection")
	}
}

func TestResolveSSHTarget_RejectsUnknownAndNoLease(t *testing.T) {
	mgr := &fakeManager{byName: map[string]*avm.VM{
		"web01": {Name: "web01", Status: avm.StatusRunning, NICs: []netfabric.NIC{
			{MAC: "52:54:00:11:22:33"},
		}},
	}}

	if _, err := resolveSSHTarget(mgr, fakeLeases{}, "web01", 22); err == nil {
		t.Errorf("resolveSSHTarget(no managed-DHCP lease) = nil error, want rejection")
	}
	if _, err := resolveSSHTarget(mgr, fakeLeases{}, "ghost", 22); err == nil {
		t.Errorf("resolveSSHTarget(unknown) = nil error, want rejection")
	}
}

// TestSpliceConns_BidirectionalThenTeardown drives spliceConns with two
// in-memory net.Pipe pairs: it proves bytes flow both ways and that closing one
// leg tears the other down (the kill-implies-teardown / no-leak invariant).
func TestSpliceConns_BidirectionalThenTeardown(t *testing.T) {
	wsLocal, wsTest := net.Pipe()
	tcpLocal, tcpTest := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spliceDone := make(chan struct{})
	go func() {
		spliceConns(ctx, cancel, wsLocal, tcpLocal)
		close(spliceDone)
	}()

	// ws -> tcp direction.
	writeWithDeadline(t, wsTest, []byte("ping"))
	if got := readWithDeadline(t, tcpTest, 4); string(got) != "ping" {
		t.Errorf("ws->tcp = %q, want %q", got, "ping")
	}

	// tcp -> ws direction.
	writeWithDeadline(t, tcpTest, []byte("pong"))
	if got := readWithDeadline(t, wsTest, 4); string(got) != "pong" {
		t.Errorf("tcp->ws = %q, want %q", got, "pong")
	}

	// Close one leg; spliceConns must tear the other down and return.
	_ = wsTest.Close()

	_ = tcpTest.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tcpTest.Read(make([]byte, 1)); err == nil {
		t.Errorf("tcpTest.Read after closing ws leg = nil error, want torn-down read error")
	}

	select {
	case <-spliceDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("spliceConns did not return after a leg closed (leak)")
	}
}

func writeWithDeadline(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write %q: %v", b, err)
	}
}

func readWithDeadline(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	got, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:got]
}
