// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStart_Defaults(t *testing.T) {
	m := Start(t, Options{})

	if m.NodeID() == uuid.Nil {
		t.Errorf("NodeID() = nil uuid; want randomised default")
	}
	if got := m.URL(); !strings.HasPrefix(got, "http://") {
		t.Errorf("URL() = %q; want http:// prefix in TLSDisabled mode", got)
	}
	if m.architecture != "amd64" {
		t.Errorf("architecture = %q; want %q", m.architecture, "amd64")
	}
}

func TestStart_TLSEnabled(t *testing.T) {
	m := Start(t, Options{TLS: TLSEnabled})

	if got := m.URL(); !strings.HasPrefix(got, "https://") {
		t.Errorf("URL() = %q; want https:// prefix in TLSEnabled mode", got)
	}
}

func TestStart_PanicOnInconsistentHeartbeatConfig(t *testing.T) {
	// HeartbeatInterval > 0 with no ControlPlaneURL is a hard
	// configuration error: the goroutine has nowhere to push.
	// ControlPlaneURL alone is permitted — tests that drive push
	// synchronously via SendHeartbeatNow can configure that way.
	fake := &fakeT{}
	defer func() {
		_ = recover()
		if !fake.failed {
			t.Errorf("Start did not call Fatalf for interval_set_url_missing")
		}
	}()
	Start(fake, Options{HeartbeatInterval: 1})
}

func TestStart_ServerResponds(t *testing.T) {
	m := Start(t, Options{})

	resp, err := http.Get(m.URL() + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStop_Idempotent(t *testing.T) {
	m := Start(t, Options{})
	m.Stop()
	m.Stop() // must not panic / deadlock.
}

// fakeT is a minimal TestingT that records Fatalf and panics so tests
// can exercise misconfiguration paths without aborting.
type fakeT struct {
	failed bool
}

func (f *fakeT) Helper()        {}
func (f *fakeT) Cleanup(func()) {}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
	panic("fakeT.Fatalf")
}
