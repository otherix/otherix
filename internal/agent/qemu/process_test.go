// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStopNBD starts a trivial long-running child, stops it via StopNBD, and
// asserts the process is gone. SIGTERM kills `sleep` outright, so the SIGKILL
// fallback is dormant here; the assertion is only that the pid no longer
// exists after StopNBD returns.
func TestStopNBD(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap the child so it does not linger as a zombie (which IsAlive would
	// still report as alive via signal 0).
	defer func() { _ = cmd.Wait() }()

	if !IsAlive(pid) {
		t.Fatalf("child pid %d not alive after Start", pid)
	}
	if err := StopNBD(pid, 2*time.Second); err != nil {
		t.Fatalf("StopNBD(%d) = %v, want nil", pid, err)
	}
	// Reap before checking liveness: a terminated-but-unwaited child is a
	// zombie that still answers signal 0.
	_ = cmd.Wait()
	if IsAlive(pid) {
		t.Errorf("pid %d still alive after StopNBD", pid)
	}
}

// TestStopNBDNoopForDeadPid confirms StopNBD is a no-op (nil) for a
// nonexistent / nonpositive pid.
func TestStopNBDNoopForDeadPid(t *testing.T) {
	if err := StopNBD(0, time.Second); err != nil {
		t.Errorf("StopNBD(0) = %v, want nil", err)
	}
	if err := StopNBD(-1, time.Second); err != nil {
		t.Errorf("StopNBD(-1) = %v, want nil", err)
	}
}

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
