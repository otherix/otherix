// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestDialQMPBoundsGreetingRead pins the reconciler-wedge fix: a second dial to
// a single-client QMP socket whose one connection is already held open (the
// live-migration case) must not block forever on the greeting read. DialQMP
// must return an error within ~dialTimeout, not hang until the far side closes.
func TestDialQMPBoundsGreetingRead(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Accept the dial but never write the QMP greeting, then hold the
	// connection well past dialTimeout - exactly what qemu does to a second
	// connection on a `server,nowait` socket that is already in use.
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		time.Sleep(3 * time.Second)
	}()

	start := time.Now()
	_, err = DialQMP(sock, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("DialQMP succeeded against a greeting-less socket, want error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("DialQMP blocked %v on a greeting-less socket, want bounded ~200ms", elapsed)
	}
}

func TestParseGuestStatsUsedMiB(t *testing.T) {
	const mib = 1024 * 1024
	tests := []struct {
		name string
		raw  string
		want *int64 // nil means "no observation"
	}{
		{
			name: "total minus free",
			// total 2048 MiB, free 512 MiB -> used 1536 MiB
			raw:  `{"return":{"last-update":1234,"stats":{"stat-total-memory":` + itoa(2048*mib) + `,"stat-free-memory":` + itoa(512*mib) + `}}}`,
			want: i64ptr(1536),
		},
		{name: "last-update zero -> nil", raw: `{"return":{"last-update":0,"stats":{"stat-total-memory":` + itoa(2048*mib) + `,"stat-free-memory":` + itoa(512*mib) + `}}}`, want: nil},
		{name: "empty stats -> nil", raw: `{"return":{"last-update":10,"stats":{}}}`, want: nil},
		{name: "missing free -> nil", raw: `{"return":{"last-update":10,"stats":{"stat-total-memory":` + itoa(2048*mib) + `}}}`, want: nil},
		{name: "zero total -> nil", raw: `{"return":{"last-update":10,"stats":{"stat-total-memory":0,"stat-free-memory":0}}}`, want: nil},
		{name: "free exceeds total (garbage) -> nil", raw: `{"return":{"last-update":10,"stats":{"stat-total-memory":` + itoa(512*mib) + `,"stat-free-memory":` + itoa(2048*mib) + `}}}`, want: nil},
		{name: "unparseable -> nil", raw: `not json`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGuestStatsUsedMiB([]byte(tc.raw))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("parseGuestStatsUsedMiB(%s) mismatch (-want +got):\n%s", tc.name, diff)
			}
		})
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func i64ptr(v int64) *int64 { return &v }
