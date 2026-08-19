// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// listenFakeSerial starts a Unix listener under /tmp (the per-test temp
// dir overflows sun_path on some platforms) that drains everything a
// serial multiplexer writes to it. It stands in for a running qemu's
// -serial chardev.
func listenFakeSerial(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "oxsock")
	if err != nil {
		t.Skipf("cannot create short socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, name+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) }(c)
		}
	}()
	return path
}

// TestManager_ByName_NewestWinsDeterministically pins the resolution
// contract for a name shared by two locally-known VMs: force-deleting a
// VM on the control plane leaves the agent running an orphan it no
// longer declares, and recreating a VM under the same name gives the
// agent two live entries with that name. ByName must always resolve to
// the newest of them; ranging over the VM map and taking the first hit
// made every call a coin flip, so a logs / console / lifecycle request
// could land on either VM.
func TestManager_ByName_NewestWinsDeterministically(t *testing.T) {
	m := newTestManager(t)

	old := m.seedStoppedVM(t, "dup")
	recent := m.seedStoppedVM(t, "dup")
	m.mu.Lock()
	old.CreatedAt = time.Now().UTC().Add(-time.Hour)
	recent.CreatedAt = time.Now().UTC()
	m.mu.Unlock()

	for i := range 100 {
		got, err := m.ByName("dup")
		if err != nil {
			t.Fatalf("ByName(dup) iteration %d: %v", i, err)
		}
		if got.ID != recent.ID {
			t.Fatalf("ByName(dup) iteration %d = %v, want %v (the newest entry)", i, got.ID, recent.ID)
		}
	}
}

// TestManager_ByName_TieBreaksOnID pins determinism when two same-named
// entries carry an identical creation timestamp: the winner is still the
// same VM on every call.
func TestManager_ByName_TieBreaksOnID(t *testing.T) {
	m := newTestManager(t)

	a := m.seedStoppedVM(t, "dup")
	b := m.seedStoppedVM(t, "dup")
	stamp := time.Now().UTC()
	m.mu.Lock()
	a.CreatedAt = stamp
	b.CreatedAt = stamp
	m.mu.Unlock()

	first, err := m.ByName("dup")
	if err != nil {
		t.Fatalf("ByName(dup): %v", err)
	}
	for i := range 100 {
		got, err := m.ByName("dup")
		if err != nil {
			t.Fatalf("ByName(dup) iteration %d: %v", i, err)
		}
		if got.ID != first.ID {
			t.Fatalf("ByName(dup) iteration %d = %v, want %v (stable tie-break)", i, got.ID, first.ID)
		}
	}
}

// TestManager_AttachMux_SameNameVMsKeepIndependentMuxes pins the
// registry key: two locally-known VMs sharing a name each keep their own
// serial multiplexer. Keying the registry by name collapsed them onto
// one slot, so starting the second VM closed the first one's multiplexer
// and console / logs served whichever VM last won the slot.
func TestManager_AttachMux_SameNameVMsKeepIndependentMuxes(t *testing.T) {
	m := newTestManager(t)

	first := m.seedStoppedVM(t, "dup")
	second := m.seedStoppedVM(t, "dup")
	m.mu.Lock()
	first.ConsoleSocket = listenFakeSerial(t, "first")
	second.ConsoleSocket = listenFakeSerial(t, "second")
	m.mu.Unlock()

	if err := m.attachMux(discardLogger(), first); err != nil {
		t.Fatalf("attachMux(first): %v", err)
	}
	firstMux := m.GetMux(first.ID)
	if firstMux == nil {
		t.Fatal("GetMux(first) = nil after attachMux")
	}
	sub := firstMux.SubscribeLogs(0, true)
	t.Cleanup(func() { _ = sub.Close() })

	if err := m.attachMux(discardLogger(), second); err != nil {
		t.Fatalf("attachMux(second): %v", err)
	}

	select {
	case <-sub.Done():
		t.Error("first VM's multiplexer closed when a same-named VM attached its own")
	case <-time.After(200 * time.Millisecond):
	}

	secondMux := m.GetMux(second.ID)
	if secondMux == nil {
		t.Fatal("GetMux(second) = nil after attachMux")
	}
	if got := m.GetMux(first.ID); got != firstMux {
		t.Errorf("GetMux(first) = %p, want %p (the first VM's own multiplexer)", got, firstMux)
	}
	if secondMux == firstMux {
		t.Error("GetMux(second) returned the first VM's multiplexer; same-named VMs must not share one")
	}

	// A genuine re-registration of the SAME VM still replaces (and
	// closes) its prior multiplexer.
	if err := m.attachMux(discardLogger(), first); err != nil {
		t.Fatalf("attachMux(first, again): %v", err)
	}
	select {
	case <-sub.Done():
	case <-time.After(2 * time.Second):
		t.Error("re-attaching the same VM left its prior multiplexer open")
	}
}
