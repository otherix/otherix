// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"os/exec"
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
