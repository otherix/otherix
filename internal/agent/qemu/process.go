// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReadPIDFile reads and parses a qemu pidfile. Returns 0 + os.ErrNotExist
// when the file is absent so callers can branch on errors.Is(err,
// os.ErrNotExist).
func ReadPIDFile(path string) (int, error) {
	// #nosec G304 -- path is the agent's own pidfile location, not user input.
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %s: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d in %s", pid, path)
	}
	return pid, nil
}

// IsAlive reports whether the process with pid currently exists. It does
// not distinguish between "running", "sleeping", and "zombie" — any
// process that responds to signal 0 counts as alive.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// Spawn invokes binary with args and waits for the process to exit. With
// qemu's -daemonize flag the parent exits 0 once the child has detached
// and the QMP / console / pid sockets are ready, so a successful return
// here means the daemonized child is fully up.
//
// On failure the qemu output (stderr+stdout of the parent) is included in
// the returned error message — qemu's exit-time diagnostics are the only
// signal callers get when daemonized invocations fail.
func Spawn(ctx context.Context, binary string, args []string) error {
	// #nosec G204 -- binary is selected from a fixed allow-list (qemu.Binary)
	// and args are constructed by qemu.BuildArgs from validated VMSpec fields.
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("spawn %s: %w: %s", binary, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// WaitGone polls until the process with pid is no longer alive, or until
// the deadline (timeout from now) elapses. Returns nil when the process
// exits, ctx.Err() when the parent context cancels, or
// context.DeadlineExceeded when timeout elapses.
func WaitGone(ctx context.Context, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !IsAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Kill sends SIGKILL to pid. Used as the last resort after a graceful
// shutdown via QMP system_powerdown + Quit times out.
func Kill(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
