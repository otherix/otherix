// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package console

import (
	"errors"
	"sync"
)

// ErrConsoleInUse signals that а concurrent WebSocket upgrade was
// requested while one is already open for the same VM. The stream
// handler returns 409 console_in_use to the caller. А second connect
// must wait for the first к close — there is no takeover / broadcast
// semantic этой iteration.
var ErrConsoleInUse = errors.New("console: console in use")

// ConnectionTracker maintains the per-VM in-memory single-session
// lock. Acquire is the atomic claim; Release the cleanup paired with
// it by deferred call in the stream handler. Both operations run
// under а single mutex — the contention window is microseconds and
// the map stays single-digit entries on а typical node, so finer
// locking would only add complexity.
type ConnectionTracker struct {
	mu     sync.Mutex
	active map[string]struct{} // key = vm name
}

// NewConnectionTracker returns an empty tracker. Tests construct
// fresh trackers per case к keep state isolated; production wires а
// single tracker into the handler via Agent.Run.
func NewConnectionTracker() *ConnectionTracker {
	return &ConnectionTracker{active: make(map[string]struct{})}
}

// Acquire claims the per-VM lock. Returns ErrConsoleInUse if а
// session is already open for vmName; the caller maps that к 409.
// Empty vmName is rejected to keep the empty-key from becoming а
// stealth shared lock under а handler bug.
func (c *ConnectionTracker) Acquire(vmName string) error {
	if vmName == "" {
		return errors.New("console: vm name is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.active[vmName]; exists {
		return ErrConsoleInUse
	}
	c.active[vmName] = struct{}{}
	return nil
}

// Release drops the per-VM lock. Idempotent — releasing а lock that
// is not held is а no-op, which keeps double-defer-on-cleanup paths
// safe.
func (c *ConnectionTracker) Release(vmName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, vmName)
}

// Active reports whether vmName currently holds the console lock.
// Exposed for test assertions; production code reads the result of
// Acquire/Release.
func (c *ConnectionTracker) Active(vmName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.active[vmName]
	return ok
}
