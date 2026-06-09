// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dnsproxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"
)

// startStubUpstream runs a UDP server that echoes back the query bytes with the
// QR (response) bit set, standing in for a real resolver. Returns its addr.
func startStubUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub upstream listen = %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := make([]byte, n)
			copy(resp, buf[:n])
			if n >= 3 {
				resp[2] |= 0x80 // set QR bit -> response
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestForwarderRelaysQuery(t *testing.T) {
	upstream := startStubUpstream(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	fwd, err := New(Config{Listen: "127.0.0.1:0", Upstreams: []string{upstream}, Log: log})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = fwd.Run(ctx) }()

	select {
	case <-fwd.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not become ready")
	}

	client, err := net.Dial("udp4", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial forwarder = %v", err)
	}
	defer client.Close()

	// Minimal DNS query header (id=0xABCD, RD set). Body is irrelevant: the stub
	// echoes and the forwarder relays bytes verbatim.
	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	if _, err := client.Write(query); err != nil {
		t.Fatalf("write query = %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, 1500)
	n, err := client.Read(resp)
	if err != nil {
		t.Fatalf("read response = %v", err)
	}
	if n < 3 || resp[0] != 0xAB || resp[1] != 0xCD {
		t.Errorf("response id mismatch: got % x", resp[:min(n, 4)])
	}
	if resp[2]&0x80 == 0 {
		t.Errorf("response QR bit not set; got %#x", resp[2])
	}
}

func TestUpstreamResolversFallback(t *testing.T) {
	// A resolv.conf with only a loopback nameserver yields the public fallback.
	dir := t.TempDir()
	p := dir + "/resolv.conf"
	if err := os.WriteFile(p, []byte("nameserver 127.0.0.53\n"), 0o600); err != nil {
		t.Fatalf("write resolv.conf = %v", err)
	}
	got := upstreamResolversFrom([]string{p})
	if len(got) != 1 || got[0] != "1.1.1.1:53" {
		t.Errorf("upstreamResolversFrom = %v, want [1.1.1.1:53]", got)
	}
}

func TestUpstreamResolversParsesNonLoopback(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/resolv.conf"
	if err := os.WriteFile(p, []byte("nameserver 127.0.0.1\nnameserver 8.8.8.8\n"), 0o600); err != nil {
		t.Fatalf("write resolv.conf = %v", err)
	}
	got := upstreamResolversFrom([]string{p})
	if len(got) != 1 || got[0] != "8.8.8.8:53" {
		t.Errorf("upstreamResolversFrom = %v, want [8.8.8.8:53]", got)
	}
}
