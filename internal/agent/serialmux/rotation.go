// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// logFileMode is the permission bits used for both the active and
// rotated log files. Serial output may include kernel messages and
// guest user input - keep it owner-only.
const logFileMode = 0o600

// rotateLogFile renames the active log inside logDir to the single-
// slot rotated path (overwriting any existing rotated file), then
// creates and returns a fresh append-mode handle to the new active
// log. A missing active log is treated as a no-op rename: rotation
// still produces a usable new handle.
//
// Callers (the multiplexer) hold a write lock around the pump's
// active handle for the duration of this call so a concurrent Write
// cannot land between Close and OpenFile.
func rotateLogFile(logDir string) (*os.File, error) {
	currentPath := filepath.Join(logDir, logFileName)
	rotatedPath := filepath.Join(logDir, rotatedFileName)

	if err := os.Rename(currentPath, rotatedPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("rename %s -> %s: %v", currentPath, rotatedPath, err)
	}

	f, err := os.OpenFile(currentPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, logFileMode) //nolint:gosec // currentPath is composed from agent-controlled logDir + fixed file name.
	if err != nil {
		return nil, fmt.Errorf("open new %s: %v", currentPath, err)
	}
	return f, nil
}
