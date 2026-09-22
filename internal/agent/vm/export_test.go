// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/qemu"
)

// NBDStopSpy records the pids of the incoming servers a Manager stopped.
type NBDStopSpy struct {
	mu   sync.Mutex
	pids []int
}

// Pids returns the stopped pids so far.
func (s *NBDStopSpy) Pids() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.pids...)
}

// NewManagerForSeamTest builds a real Manager over a temp state dir with the
// process seams stubbed: StartIncoming creates no disk image and spawns no
// qemu-nbd, and every server stop is recorded instead of signalled.
func NewManagerForSeamTest(t *testing.T) (*Manager, *NBDStopSpy) {
	t.Helper()
	m := newTestManager(t)
	spy := &NBDStopSpy{}
	next := 1000
	m.migCreateDisk = func(context.Context, string, int64) error { return nil }
	m.migSpawnNBD = func(context.Context, []string) (*qemu.NBDServer, error) {
		next++
		return &qemu.NBDServer{Pid: next}, nil
	}
	m.migStopNBD = func(srv *qemu.NBDServer, _ time.Duration) error {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		spy.pids = append(spy.pids, srv.Pid)
		return nil
	}
	return m, spy
}

// DefaultPoolForSeamTest is the pool newTestManager registered.
func (m *Manager) DefaultPoolForSeamTest() string { return m.defaultTestPool() }
