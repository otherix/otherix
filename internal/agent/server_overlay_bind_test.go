// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPrimaryControlBind(t *testing.T) {
	tests := []struct {
		name               string
		listen             string
		advertisedEndpoint string
		want               string
	}{
		{
			name:               "natd node forced off wildcard to loopback",
			listen:             "0.0.0.0:9443",
			advertisedEndpoint: "",
			want:               "127.0.0.1:9443",
		},
		{
			name:               "natd node empty-host listen forced to loopback",
			listen:             ":9443",
			advertisedEndpoint: "",
			want:               "127.0.0.1:9443",
		},
		{
			name:               "natd node ipv6 wildcard forced to loopback",
			listen:             "[::]:9443",
			advertisedEndpoint: "",
			want:               "127.0.0.1:9443",
		},
		{
			name:               "public node unchanged",
			listen:             "0.0.0.0:9443",
			advertisedEndpoint: "203.0.113.5:51820",
			want:               "0.0.0.0:9443",
		},
		{
			name:               "public node explicit host unchanged",
			listen:             "10.0.0.5:9443",
			advertisedEndpoint: "203.0.113.5:51820",
			want:               "10.0.0.5:9443",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := primaryControlBind(tc.listen, tc.advertisedEndpoint)
			if err != nil {
				t.Fatalf("primaryControlBind(%q, %q) error = %v", tc.listen, tc.advertisedEndpoint, err)
			}
			if got != tc.want {
				t.Errorf("primaryControlBind(%q, %q) = %q, want %q", tc.listen, tc.advertisedEndpoint, got, tc.want)
			}
		})
	}
}

func TestPrimaryControlBind_RejectsMalformedListen(t *testing.T) {
	if _, err := primaryControlBind("not-an-addr", ""); err == nil {
		t.Error("primaryControlBind(malformed, natd) error = nil, want error")
	}
}

func TestListenOverlayControl(t *testing.T) {
	// A known overlay IP yields a bound listener on overlayIP:port; the CP reaches
	// a NAT'd agent here through the gateway splice.
	ln, err := listenOverlayControl(netip.MustParseAddr("127.0.0.1"), "0")
	if err != nil {
		t.Fatalf("listenOverlayControl: %v", err)
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr type = %T, want *net.TCPAddr", ln.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Errorf("listener IP = %v, want loopback", addr.IP)
	}
	// The listener is really up: a TCP dial to it connects.
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial overlay listener: %v", err)
	}
	_ = c.Close()
}

func TestListenOverlayControl_UnbindableIPRetries(t *testing.T) {
	// An overlay IP not assigned to any local interface fails the bind, which the
	// supervisor treats as retryable (fail toward inaction, never crash).
	if _, err := listenOverlayControl(netip.MustParseAddr("192.0.2.1"), "0"); err == nil {
		t.Error("listenOverlayControl(non-local IP) error = nil, want bind error")
	}
}

// TestStartOverlayControlListener_NatdBindsAndDrains drives the supervisor for a
// NAT'd node: it must enter the loop, consult the overlay-IP source, keep its
// done channel open while serving, and close it once the context is cancelled.
func TestStartOverlayControlListener_NatdBindsAndDrains(t *testing.T) {
	var calls atomic.Int32
	overlayIP := func() (netip.Addr, bool) {
		calls.Add(1)
		return netip.MustParseAddr("127.0.0.1"), true
	}
	cfg := &config.AgentConfig{}
	cfg.Server.ShutdownGrace = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := startOverlayControlListener(ctx, overlayIP, "0", nil, nil, cfg, quietLogger())

	// The supervisor blocks serving/waiting: done must not close until we cancel.
	select {
	case <-done:
		t.Fatal("NAT'd node: done closed before cancel; supervisor exited early")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NAT'd node: done not closed after cancel")
	}

	// Reading calls after done is safe: close(done) happens-after the loop body.
	if calls.Load() == 0 {
		t.Error("NAT'd node: overlayIP never consulted; supervisor skipped the bind loop")
	}
}

// TestStartOverlayControlListener_PublicSkips is the seam guard: a public node
// (non-empty advertised WireGuard endpoint) must never start the overlay
// listener - it returns an already-closed channel and never consults the
// overlay-IP source. Reverting the public-node guard makes this fail (the
// supervisor would enter the loop, call overlayIP, and block until cancel).
func TestStartOverlayControlListener_PublicSkips(t *testing.T) {
	var calls atomic.Int32
	overlayIP := func() (netip.Addr, bool) {
		calls.Add(1)
		return netip.MustParseAddr("127.0.0.1"), true
	}
	cfg := &config.AgentConfig{}
	cfg.WireGuard.AdvertisedEndpoint = "203.0.113.5:51820"
	cfg.Server.ShutdownGrace = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startOverlayControlListener(ctx, overlayIP, "9443", nil, nil, cfg, quietLogger())

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("public node: done not closed promptly; overlay listener was not skipped")
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("public node: overlayIP consulted %d times, want 0 (listener must be skipped)", n)
	}
}
