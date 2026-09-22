// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// writeFakeProc lays out <root>/<pid>/cmdline with the NUL-separated args a
// /proc entry would expose, mirroring the kernel's argv[] encoding qemu is
// launched with.
func writeFakeProc(t *testing.T, root string, pid int, args ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data := strings.Join(args, "\x00")
	if len(args) > 0 {
		data += "\x00" // /proc/<pid>/cmdline is NUL-terminated, not just NUL-separated
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(data), 0o600); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}

// TestVerifyCmdlineAt drives the identity check over a synthetic /proc: a pid is
// only "ours" when its cmdline carries the exact `-uuid <vmUUID>` pair qemu is
// launched with. Every other shape (different uuid, missing flag, absent entry,
// dangling flag, non-positive pid, empty uuid) must be rejected so the caller
// treats it as not-alive and never signals it.
func TestVerifyCmdlineAt(t *testing.T) {
	const vmUUID = "11111111-1111-1111-1111-111111111111"
	root := t.TempDir()

	writeFakeProc(t, root, 100, "qemu-system-x86_64", "-name", "vm", "-uuid", vmUUID, "-pidfile", "/x")
	writeFakeProc(t, root, 101, "qemu-system-x86_64", "-name", "vm", "-uuid", "22222222-2222-2222-2222-222222222222")
	writeFakeProc(t, root, 102, "/usr/bin/some-other-daemon", "--serve")
	writeFakeProc(t, root, 103, "qemu-system-x86_64", "-name", "vm", "-uuid") // dangling flag, no value

	cases := []struct {
		name string
		pid  int
		uuid string
		want bool
	}{
		{"our qemu matches", 100, vmUUID, true},
		{"reused pid different uuid", 101, vmUUID, false},
		{"unrelated process", 102, vmUUID, false},
		{"dangling uuid flag", 103, vmUUID, false},
		{"process gone (no proc entry)", 999, vmUUID, false},
		{"non-positive pid", 0, vmUUID, false},
		{"negative pid", -5, vmUUID, false},
		{"empty uuid", 100, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyCmdlineAt(root, tc.pid, tc.uuid); got != tc.want {
				t.Errorf("verifyCmdlineAt(%q, %d, %q) = %v, want %v", root, tc.pid, tc.uuid, got, tc.want)
			}
		})
	}
}

// signalSpy records the signals sent through a handle. exitOn, when non-zero,
// is the signal after which the fake process counts as gone: every later
// signal returns os.ErrProcessDone.
type signalSpy struct {
	mu     sync.Mutex
	sent   []os.Signal
	exitOn syscall.Signal
	gone   bool
}

func (s *signalSpy) Signal(sig os.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return os.ErrProcessDone
	}
	s.sent = append(s.sent, sig)
	if s.exitOn != 0 && sig == s.exitOn {
		s.gone = true
	}
	return nil
}

func (s *signalSpy) signals() []os.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]os.Signal(nil), s.sent...)
}

// TestStopNBDStopsItsOwnChildPromptly spawns a real child through the same path
// SpawnQemuNBD uses and stops it: the stop returns well inside its grace and the
// child is reaped.
func TestStopNBDStopsItsOwnChildPromptly(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	srv, err := spawnNBD("sleep", []string{"30"})
	if err != nil {
		t.Fatalf("spawnNBD: %v", err)
	}
	start := time.Now()
	if err := StopNBD(srv, 2*time.Second); err != nil {
		t.Fatalf("StopNBD = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("StopNBD took %v, want well under its 2s grace", elapsed)
	}
	select {
	case <-srv.done:
	case <-time.After(time.Second):
		t.Errorf("child %d not reaped after StopNBD", srv.Pid)
	}
}

// TestStopNBDAfterExitIsNoop: stopping a server that already exited returns nil
// without error. That it sends nothing is the Go runtime's guarantee for a
// reaped process handle, not something this test can observe.
func TestStopNBDAfterExitIsNoop(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skipf("true not available: %v", err)
	}
	srv, err := spawnNBD("true", nil)
	if err != nil {
		t.Fatalf("spawnNBD: %v", err)
	}
	<-srv.done
	if err := StopNBD(srv, time.Second); err != nil {
		t.Errorf("StopNBD after exit = %v, want nil", err)
	}
}

// TestStopNBDEscalatesToKill: a server that ignores SIGTERM is SIGKILLed once
// the grace runs out, through the same handle.
func TestStopNBDEscalatesToKill(t *testing.T) {
	spy := &signalSpy{exitOn: syscall.SIGKILL}
	if err := StopNBD(&NBDServer{Pid: 42, proc: spy}, 300*time.Millisecond); err != nil {
		t.Fatalf("StopNBD = %v, want nil", err)
	}
	sent := spy.signals()
	if len(sent) < 2 || sent[0] != syscall.SIGTERM || sent[len(sent)-1] != syscall.SIGKILL {
		t.Errorf("signals = %v, want SIGTERM first and SIGKILL last", sent)
	}
}

// TestStopNBDWithoutProcessIsNoop covers nil and the zero handle test fakes use.
func TestStopNBDWithoutProcessIsNoop(t *testing.T) {
	if err := StopNBD(nil, time.Second); err != nil {
		t.Errorf("StopNBD(nil) = %v, want nil", err)
	}
	if err := StopNBD(&NBDServer{Pid: 4321}, time.Second); err != nil {
		t.Errorf("StopNBD(zero handle) = %v, want nil", err)
	}
}
