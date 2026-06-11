// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dnsproxy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// TestRelayRecoversFromExchangePanic is the teeth for relay's panic recovery:
// it forces an actual panic through the dnsExchange seam and asserts relay
// returns normally and logs the drop. With the recover removed, the panic
// escapes relay and crashes the test binary, so this test cannot pass
// vacuously. Not parallel: it mutates the package-level seam.
func TestRelayRecoversFromExchangePanic(t *testing.T) {
	orig := dnsExchange
	defer func() { dnsExchange = orig }()
	dnsExchange = func(upstream string, query []byte, timeout time.Duration) ([]byte, error) {
		panic("boom")
	}

	var buf bytes.Buffer
	f := &Forwarder{
		log:       slog.New(slog.NewTextHandler(&buf, nil)),
		upstreams: []string{"192.0.2.1:53"},
	}

	// conn and client may be nil: the panic fires inside dnsExchange before
	// relay touches conn.WriteTo.
	f.relay(nil, nil, []byte{0x12, 0x34})

	// Reaching this line means the panic did not propagate out of relay.
	if got := buf.String(); !strings.Contains(got, "panicked") {
		t.Errorf("relay log = %q, want it to contain %q", got, "panicked")
	}
}

// startHostileUpstream runs a UDP server that answers every query with the
// fixed payload resp (which may be empty), standing in for a malicious or
// broken resolver. Returns its addr.
func startHostileUpstream(t *testing.T, resp []byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hostile upstream listen = %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			_, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

// TestRelayHostileBytes feeds hostile byte sequences through the REAL
// exchange path and asserts no panic propagates. This documents real-input
// safety only; it is explicitly NOT the teeth for the recover (exchangeUDP
// returns errors or filters these inputs rather than panicking).
// TestRelayRecoversFromExchangePanic is the test that fails without the
// recover.
func TestRelayHostileBytes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("hostile_query_full_relay", func(t *testing.T) {
		// A hostile guest query through the full relay path. The upstream
		// echoes it back, so the txid matches and relay proceeds to the
		// write-back. Must return normally.
		query := bytes.Repeat([]byte{0xff}, 64)
		upstream := startHostileUpstream(t, query)

		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("relay conn listen = %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		clientConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("client listen = %v", err)
		}
		t.Cleanup(func() { _ = clientConn.Close() })

		f := &Forwarder{log: log, upstreams: []string{upstream}}
		f.relay(conn, clientConn.LocalAddr(), query)
		// Reaching this line means relay returned normally.
	})

	// Hostile upstream RESPONSES through the real seam target, exercising the
	// same parse path relay uses. Called directly with a short timeout because
	// relay hardcodes a 3s exchange timeout and responses that cannot match
	// the txid only resolve at the deadline.
	query := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	cases := map[string][]byte{
		"empty_response":     {},
		"one_byte_response":  {0xff},
		"ff_64_response":     bytes.Repeat([]byte{0xff}, 64),
		"truncated_response": {0x12, 0x34, 0x80},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := startHostileUpstream(t, resp)
			_, _ = exchangeUDP(upstream, query, 250*time.Millisecond)
			// Reaching this line means exchangeUDP returned normally
			// (matched-txid responses return bytes, the rest time out).
		})
	}
}
