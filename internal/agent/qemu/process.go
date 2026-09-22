// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
//
// IsAlive answers "does this pid exist", NOT "is this pid OUR qemu". A pidfile
// pid can be reused by an unrelated process after the recorded qemu dies
// (across a reboot, or after an OOM kill), so a positive IsAlive on a pidfile
// pid is not sufficient to report a VM running or to send it a signal. Use
// VerifyCmdline for that identity check; reserve IsAlive for pids the agent
// still owns in-process (freshly spawned children, migration helper pids).
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// VerifyCmdline reports whether the process with pid is the qemu the agent
// launched for the VM identified by vmUUID: it reads /proc/<pid>/cmdline and
// requires the `-uuid <vmUUID>` argument pair qemu is always launched with
// (cmdline.go, migrations_live.go). It subsumes a liveness check — a dead pid
// has no /proc entry, and a reaped-but-unwaited zombie has an empty cmdline —
// so a true result means "alive AND ours".
//
// Any read/parse failure, an absent /proc entry, a missing or mismatched uuid,
// a non-positive pid, or an empty vmUUID returns false. Callers MUST treat
// false as "not our process": downgrade an observed `running` to stopped, and
// NEVER send a signal (the pid may belong to an unrelated process after PID
// reuse). This is the pidfile trust boundary — fail toward inaction.
func VerifyCmdline(pid int, vmUUID string) bool {
	return verifyCmdlineAt("/proc", pid, vmUUID)
}

// verifyCmdlineAt is VerifyCmdline with an injectable proc root for testing.
func verifyCmdlineAt(procRoot string, pid int, vmUUID string) bool {
	if pid <= 0 || vmUUID == "" {
		return false
	}
	// #nosec G304 -- procRoot is a constant ("/proc") in production; pid is the
	// agent's own pidfile value, not user input.
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	// /proc/<pid>/cmdline is the process argv[] joined by NUL bytes. Find a
	// `-uuid` token immediately followed by the VM's uuid.
	args := strings.Split(string(data), "\x00")
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-uuid" && args[i+1] == vmUUID {
			return true
		}
	}
	return false
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

// StopNBD stops a qemu-nbd server gracefully: SIGTERM (it closes the export,
// flushes, and releases the disk write lock), then SIGKILL if it has not exited
// within grace, and waits for that exit too. Both signals go through the
// process handle, so a server that already exited is a no-op rather than a
// signal to whatever reused its pid.
// A nil server, or one without a process, is a no-op.
func StopNBD(srv *NBDServer, grace time.Duration) error {
	if srv == nil || srv.proc == nil {
		return nil
	}
	if err := srv.proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("sigterm qemu-nbd %d: %v", srv.Pid, err)
	}
	if srv.waitExit(grace) {
		return nil
	}
	if err := srv.proc.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("sigkill qemu-nbd %d: %v", srv.Pid, err)
	}
	// Wait for the exit even after SIGKILL: a caller about to open the same disk
	// must not race a server still releasing its write lock.
	if !srv.waitExit(nbdKillGrace) {
		return fmt.Errorf("qemu-nbd %d still running after SIGKILL", srv.Pid)
	}
	return nil
}

// nbdKillGrace bounds the wait for a SIGKILLed server to exit.
const nbdKillGrace = 2 * time.Second

// waitExit reports whether the server exited within grace. A server this agent
// spawned closes done when its reaper returns. A server the agent did not spawn
// (an orphan found at startup) is reaped by its new parent, so its handle is
// polled with signal 0 until the handle reports it gone. Only os.ErrProcessDone
// counts as an exit: any other probe error leaves the process not known to have
// exited, and polling continues until grace runs out.
func (s *NBDServer) waitExit(grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	if s.done != nil {
		select {
		case <-s.done:
			return true
		case <-timer.C:
			return false
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := s.proc.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
			return true
		}
		select {
		case <-timer.C:
			return false
		case <-tick.C:
		}
	}
}
