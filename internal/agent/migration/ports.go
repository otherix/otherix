// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migration holds the agent-side, in-memory bookkeeping for an
// in-flight VM migration: a record store and a port allocator over the
// ADR 0013 ingress range. Nothing here is persisted - the CP holds the
// durable record and re-drives on agent restart (fail-safe to source).
package migration

import (
	"errors"
	"sync"
)

// ErrNoFreePort is returned by PortAllocator.Reserve when every port in
// the configured range is in use. The caller surfaces it as a retryable
// capacity condition (the migration sits pending), not a hard failure.
var ErrNoFreePort = errors.New("migration: no free port in range")

// PortAllocator hands out distinct ports from an inclusive [start, end]
// range, one per concurrent incoming migration. multifd uses a single
// listening port (N connections, not N ports), so one reservation covers
// a migration regardless of channel count. Safe for concurrent use.
type PortAllocator struct {
	start int
	end   int

	mu   sync.Mutex
	used map[int]bool
}

// NewPortAllocator returns an allocator over the inclusive [start, end] range.
func NewPortAllocator(start, end int) *PortAllocator {
	return &PortAllocator{start: start, end: end, used: make(map[int]bool)}
}

// Reserve returns a free port and marks it used, or ErrNoFreePort if the
// range is exhausted.
func (a *PortAllocator) Reserve() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for p := a.start; p <= a.end; p++ {
		if !a.used[p] {
			a.used[p] = true
			return p, nil
		}
	}
	return 0, ErrNoFreePort
}

// Release returns a port to the pool. A port outside the range or not
// currently reserved is ignored.
func (a *PortAllocator) Release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, port)
}
