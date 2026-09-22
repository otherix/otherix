// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package qemu

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdHandle is a ProcessHandle over a Linux pidfd.
type pidfdHandle struct{ fd int }

// OpenPidfd opens a pidfd on pid. Used instead of os.FindProcess, which falls
// back to a raw-pid handle on any pidfd error and would then signal by pid.
func OpenPidfd(pid int) (ProcessHandle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	return pidfdHandle{fd: fd}, nil
}

// Signal sends sig through the pidfd; a process that has exited reports
// os.ErrProcessDone.
func (h pidfdHandle) Signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %v", sig)
	}
	if err := unix.PidfdSendSignal(h.fd, s, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// Release closes the pidfd.
func (h pidfdHandle) Release() error { return unix.Close(h.fd) }
