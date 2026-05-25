// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// logFileName is the active multiplexer log file inside a VM's
	// state directory; rotation moves it to rotatedFileName on the
	// 10 MB threshold (see ADR 0029, L1).
	logFileName = "serial.log"

	// rotatedFileName is the single rotated log retained alongside
	// the current file. There is no .2 / .3 - total per-VM cap is
	// ~20 MB.
	rotatedFileName = "serial.log.1"
)

// readTailLines returns the trailing n lines of the multiplexer's
// on-disk history in chronological order: any rotated bytes from
// serial.log.1 precede the current bytes from serial.log.
//
// n < 0 returns every available byte; n == 0 returns an empty slice;
// n > 0 returns at most n trailing logical lines, where "line" matches
// the convention in lastNBytes (bounded by '\n' or end-of-data).
//
// A missing log file (either current or rotated) is treated as empty;
// only unexpected I/O errors surface to the caller.
func readTailLines(logDir string, n int) ([]byte, error) {
	if n == 0 {
		return []byte{}, nil
	}
	rotated, err := readFileIfExists(filepath.Join(logDir, rotatedFileName))
	if err != nil {
		return nil, fmt.Errorf("read rotated log: %v", err)
	}
	current, err := readFileIfExists(filepath.Join(logDir, logFileName))
	if err != nil {
		return nil, fmt.Errorf("read current log: %v", err)
	}
	combined := make([]byte, 0, len(rotated)+len(current))
	combined = append(combined, rotated...)
	combined = append(combined, current...)
	if n < 0 {
		return combined, nil
	}
	return lastNBytes(combined, n), nil
}

// readFileIfExists returns the contents of path, or a nil slice when
// the file does not exist. Every other error is returned verbatim.
func readFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is composed from agent-controlled logDir + fixed file names.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// lastNBytes returns the trailing n lines of data, treating each '\n'
// as a line terminator and allowing the buffer to end on a partial
// line. n <= 0 yields empty; n exceeding the line count yields the
// whole buffer. The returned slice is a fresh allocation.
//
// Shared between RingBuffer.LastNLines (in-memory recent history) and
// readTailLines (on-disk history) so both paths apply identical line
// semantics.
func lastNBytes(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return []byte{}
	}
	lines := bytes.SplitAfter(data, []byte{'\n'})
	if last := len(lines) - 1; last >= 0 && len(lines[last]) == 0 {
		lines = lines[:last]
	}
	if n >= len(lines) {
		out := make([]byte, len(data))
		copy(out, data)
		return out
	}
	return bytes.Join(lines[len(lines)-n:], nil)
}
