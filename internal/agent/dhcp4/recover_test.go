// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// TestHandleRecoversFromParsePanic is the teeth for handle's panic recovery:
// it forces an actual panic through the dhcpParse seam and asserts handle
// returns normally and logs the drop. With the recover removed, the panic
// escapes handle and crashes the test binary, so this test cannot pass
// vacuously. Not parallel: it mutates the package-level seam.
func TestHandleRecoversFromParsePanic(t *testing.T) {
	orig := dhcpParse
	defer func() { dhcpParse = orig }()
	dhcpParse = func(payload []byte) (*dhcpv4.DHCPv4, error) { panic("boom") }

	var buf bytes.Buffer
	r := &responder{log: slog.New(slog.NewTextHandler(&buf, nil))}
	// dhcpParse is the first call in handle, so the panic fires before any
	// snapshot access and a minimal bridgeServer suffices.
	srv := &bridgeServer{bridge: "test"}

	r.handle(srv, []byte{0x01})

	// Reaching this line means the panic did not propagate out of handle.
	if got := buf.String(); !strings.Contains(got, "panicked") {
		t.Errorf("handle log = %q, want it to contain %q", got, "panicked")
	}
}

// TestHandleHostilePayloads feeds a corpus of hostile guest payloads through
// the REAL parser and asserts handle returns normally. This documents
// real-input safety only; it is explicitly NOT the teeth for the recover
// (dhcpv4.FromBytes returns errors for these inputs rather than panicking).
// TestHandleRecoversFromParsePanic is the test that fails without the recover.
func TestHandleHostilePayloads(t *testing.T) {
	r := &responder{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := &bridgeServer{bridge: "test"}
	// An empty snapshot keeps handle safe should any payload parse: the MAC
	// lookup misses and the frame is dropped.
	srv.snap.Store(&snapshot{byMAC: map[string]Reservation{}})

	valid, err := dhcpv4.New()
	if err != nil {
		t.Fatalf("dhcpv4.New() = %v", err)
	}
	truncated := valid.ToBytes()[:50]

	cases := map[string][]byte{
		"empty":     {},
		"one_byte":  {0xff},
		"ff_64":     bytes.Repeat([]byte{0xff}, 64),
		"truncated": truncated,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			r.handle(srv, payload)
			// Reaching this line means handle returned normally.
		})
	}
}
