// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProcessHandle is a handle on a process that pins its identity: a signal sent
// through it reaches that process or reports it gone, never a later process that
// reused its pid. Release frees the handle.
type ProcessHandle interface {
	Signaler
	Release() error
}

// ProcessOpener opens a ProcessHandle on pid.
type ProcessOpener func(pid int) (ProcessHandle, error)

// MatchesMigrationNBD reports whether argv is a qemu-nbd this agent spawned for
// an incoming migration: argv[0] is qemu-nbd and it carries the tls-creds-x509
// object whose dir is <stateDir>/migrations/<migration uuid>/tls, exactly as
// NBDServerArgs writes it. The directory is compared component by component,
// never as a string prefix, so one agent's state path never matches another's
// that merely starts with it.
func MatchesMigrationNBD(argv []string, stateDir string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "qemu-nbd" {
		return false
	}
	migrations := filepath.Join(stateDir, "migrations")
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "--object" {
			continue
		}
		dir, ok := tlsCredsDir(argv[i+1])
		if !ok || filepath.Base(dir) != "tls" {
			continue
		}
		idDir := filepath.Dir(dir)
		if filepath.Dir(idDir) != migrations {
			continue
		}
		if _, err := uuid.Parse(filepath.Base(idDir)); err == nil {
			return true
		}
	}
	return false
}

// tlsCredsDir returns the dir= value of a tls-creds-x509 object argument.
func tlsCredsDir(obj string) (string, bool) {
	parts := strings.Split(obj, ",")
	if parts[0] != "tls-creds-x509" {
		return "", false
	}
	for _, p := range parts[1:] {
		if v, ok := strings.CutPrefix(p, "dir="); ok {
			return filepath.Clean(v), true
		}
	}
	return "", false
}

// SweepOrphanNBD stops every migration qemu-nbd this agent left behind and
// returns how many it stopped. It must run before the agent accepts any
// migration: the records that tracked these servers lived in memory and did not
// survive the restart, and the servers outlive the agent (its unit kills only
// the agent process, so that guests survive it). Left running, a server keeps
// its disk's write lock - the migrated VM then cannot start - and its port,
// which the fresh allocator would hand out again.
//
// A candidate is matched on its command line, then opened, then matched again:
// the handle pins the process, so a pid reused between the two reads fails the
// second one and is never signalled. A process that cannot be opened is skipped
// rather than signalled by raw pid. Best-effort: nothing here fails startup.
func SweepOrphanNBD(procRoot, stateDir string, open ProcessOpener, grace time.Duration, log *slog.Logger) int {
	if !filepath.IsAbs(stateDir) {
		log.Warn("orphaned qemu-nbd sweep skipped: state path is not absolute", "state_dir", stateDir)
		return 0
	}
	stateDir = filepath.Clean(stateDir)
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Debug("orphaned qemu-nbd sweep skipped: no proc filesystem", "proc_root", procRoot)
		} else {
			log.Warn("orphaned qemu-nbd sweep skipped: read proc", "err", err)
		}
		return 0
	}
	stopped := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || !ourNBD(procRoot, e.Name(), stateDir) {
			continue
		}
		if sweepOne(procRoot, e.Name(), pid, stateDir, open, grace, log) {
			stopped++
		}
	}
	return stopped
}

// sweepOne opens pid, re-checks it, and stops it. Reports whether it stopped it.
func sweepOne(procRoot, entry string, pid int, stateDir string, open ProcessOpener, grace time.Duration, log *slog.Logger) bool {
	h, err := open(pid)
	if err != nil {
		log.Warn("orphaned qemu-nbd left running: cannot open a handle on it", "pid", pid, "err", err)
		return false
	}
	defer func() { _ = h.Release() }()
	if !ourNBD(procRoot, entry, stateDir) {
		return false
	}
	if err := StopNBD(&NBDServer{Pid: pid, proc: h}, grace); err != nil {
		log.Warn("stop orphaned migration qemu-nbd", "pid", pid, "err", err)
		return false
	}
	log.Info("stopped orphaned migration qemu-nbd", "pid", pid)
	return true
}

// ourNBD reads /proc/<entry>/cmdline and matches it.
func ourNBD(procRoot, entry, stateDir string) bool {
	// #nosec G304 -- procRoot is /proc in production; entry comes from its listing.
	data, err := os.ReadFile(filepath.Join(procRoot, entry, "cmdline"))
	if err != nil {
		return false
	}
	return MatchesMigrationNBD(strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), stateDir)
}
