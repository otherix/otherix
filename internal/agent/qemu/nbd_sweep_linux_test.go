// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package qemu

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestOpenPidfdStopsARealOrphanCandidate opens a pidfd on a real child and stops
// it through the handle; after exit the handle reports the process gone.
func TestOpenPidfdStopsARealOrphanCandidate(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	h, err := OpenPidfd(cmd.Process.Pid)
	if err != nil {
		t.Skipf("pidfd unavailable: %v", err)
	}
	defer func() { _ = h.Release() }()
	if err := StopNBD(&NBDServer{Pid: cmd.Process.Pid, proc: h}, 2*time.Second); err != nil {
		t.Fatalf("StopNBD = %v", err)
	}
	if err := h.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Signal(0) after stop = %v, want os.ErrProcessDone", err)
	}
}
