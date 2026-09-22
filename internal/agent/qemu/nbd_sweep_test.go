// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const sweepMigID = "3f2a1c9e-8b7d-4e6f-a5c4-1b2d3e4f5a6b"

func nbdArgv(stateDir string) []string {
	return append([]string{"/usr/bin/qemu-nbd"}, NBDServerArgs(NBDServerSpec{
		CredsDir: stateDir + "/migrations/" + sweepMigID + "/tls", SourceIdentity: "CN=node-src",
		BindHost: "10.0.0.2", Port: 49152, Export: sweepMigID, DiskPath: "/pool/vms/x/disk.qcow2",
	})...)
}

func discardSweepLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMatchesMigrationNBD(t *testing.T) {
	const state = "/var/lib/otherix/dev/node-1/vms"
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "this agent's server", argv: nbdArgv(state), want: true},
		{name: "another agent's state path", argv: nbdArgv("/var/lib/otherix/dev/node-2/vms")},
		{name: "a state path that only shares a prefix", argv: nbdArgv("/var/lib/otherix/dev/node-10/vms")},
		{name: "another binary carrying the path", argv: append([]string{"/bin/bash"}, nbdArgv(state)[1:]...)},
		{name: "qemu-nbd without the creds object", argv: []string{"qemu-nbd", "--persistent", "/pool/disk.qcow2"}},
		{name: "creds dir that is not a migration id", argv: []string{"qemu-nbd", "--object", "tls-creds-x509,id=migtls,endpoint=server,dir=" + state + "/migrations/not-a-uuid/tls"}},
		{name: "creds dir under a migration id that is not tls", argv: []string{"qemu-nbd", "--object", "tls-creds-x509,id=migtls,endpoint=server,dir=" + state + "/migrations/" + sweepMigID + "/other"}},
		{name: "empty argv"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesMigrationNBD(tc.argv, state); got != tc.want {
				t.Errorf("MatchesMigrationNBD(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// handleSpy is a ProcessHandle that records signals (exiting on SIGTERM) and
// whether it was released.
type handleSpy struct {
	signalSpy
	released bool
}

func (h *handleSpy) Release() error { h.released = true; return nil }

// TestSweepOrphanNBDStopsOnlyThisAgentsServers lays out a fake /proc with this
// agent's orphaned server, another agent's server and an unrelated process, and
// checks that only the first is opened and signalled, and every opened handle is
// released.
func TestSweepOrphanNBDStopsOnlyThisAgentsServers(t *testing.T) {
	const state = "/var/lib/otherix/dev/node-1/vms"
	root := t.TempDir()
	writeFakeProc(t, root, 101, nbdArgv(state)...)
	writeFakeProc(t, root, 102, nbdArgv("/var/lib/otherix/dev/node-10/vms")...)
	writeFakeProc(t, root, 103, "sleep", "30")
	// A non-numeric entry is not a process, even with a matching command line.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o750); err != nil {
		t.Fatalf("mkdir self: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "self", "cmdline"), []byte(strings.Join(nbdArgv(state), "\x00")+"\x00"), 0o600); err != nil {
		t.Fatalf("write self cmdline: %v", err)
	}

	opened := map[int]*handleSpy{}
	open := func(pid int) (ProcessHandle, error) {
		h := &handleSpy{signalSpy: signalSpy{exitOn: syscall.SIGTERM}}
		opened[pid] = h
		return h, nil
	}

	if got := SweepOrphanNBD(root, state, open, time.Second, discardSweepLog()); got != 1 {
		t.Errorf("SweepOrphanNBD stopped %d, want 1", got)
	}
	if len(opened) != 1 {
		t.Errorf("opened %d handles, want only pid 101's", len(opened))
	}
	h := opened[101]
	if h == nil || len(h.signals()) == 0 || h.signals()[0] != syscall.SIGTERM || !h.released {
		t.Errorf("pid 101: handle=%+v, want SIGTERM sent and the handle released", h)
	}
	for _, pid := range []int{102, 103} {
		if opened[pid] != nil {
			t.Errorf("pid %d opened, want it filtered out before any handle is taken", pid)
		}
	}
}

// TestSweepOrphanNBDRechecksAfterOpening: a pid reused between the first read and
// the open is caught by the second read, and the new process is not signalled.
func TestSweepOrphanNBDRechecksAfterOpening(t *testing.T) {
	const state = "/var/lib/otherix/vms"
	root := t.TempDir()
	writeFakeProc(t, root, 201, nbdArgv(state)...)

	var h *handleSpy
	open := func(pid int) (ProcessHandle, error) {
		writeFakeProc(t, root, pid, "sleep", "30") // the pid now belongs to something else
		h = &handleSpy{}
		return h, nil
	}

	if got := SweepOrphanNBD(root, state, open, time.Second, discardSweepLog()); got != 0 {
		t.Errorf("SweepOrphanNBD stopped %d, want 0", got)
	}
	if h == nil || len(h.signals()) != 0 || !h.released {
		t.Errorf("handle=%+v, want opened, released and never signalled", h)
	}
}

func TestSweepOrphanNBDSkipsWhatItCannotOpen(t *testing.T) {
	const state = "/var/lib/otherix/vms"
	root := t.TempDir()
	writeFakeProc(t, root, 301, nbdArgv(state)...)
	open := func(int) (ProcessHandle, error) { return nil, errors.New("pidfd_open: EMFILE") }

	if got := SweepOrphanNBD(root, state, open, time.Second, discardSweepLog()); got != 0 {
		t.Errorf("SweepOrphanNBD stopped %d, want 0 (fail closed when no handle can be taken)", got)
	}
}

func TestSweepOrphanNBDSkipsARelativeStatePath(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, 401, nbdArgv("vms")...)
	called := false
	open := func(int) (ProcessHandle, error) { called = true; return &handleSpy{}, nil }
	if got := SweepOrphanNBD(root, "vms", open, time.Second, discardSweepLog()); got != 0 || called {
		t.Errorf("SweepOrphanNBD(relative) = %d, opener called = %v, want 0 and not called", got, called)
	}
}

func TestSweepOrphanNBDToleratesAMissingProcRoot(t *testing.T) {
	open := func(int) (ProcessHandle, error) { return &handleSpy{}, nil }
	if got := SweepOrphanNBD(t.TempDir()+"/absent", "/var/lib/otherix/vms", open, time.Second, discardSweepLog()); got != 0 {
		t.Errorf("SweepOrphanNBD = %d, want 0", got)
	}
}
