// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	// pumpBufferSize bounds a single read from the QEMU serial socket;
	// matches the agent's prior direct-pump value (4 KiB) so behavior
	// under boot bursts stays consistent.
	pumpBufferSize = 4096

	// rotationThresholdBytes is the size at which the active log
	// triggers rotation. Locked at 10 MB per L1; tests can override
	// via newMuxWithThreshold to avoid writing 10 MB to disk.
	rotationThresholdBytes int64 = 10 * 1024 * 1024

	// defaultSubscriberCapacity sizes each subscriber's outbound
	// channel. 32 absorbs bursty boot output before the slow-consumer
	// drop path kicks in (L2).
	defaultSubscriberCapacity = 32

	// ringBufferBytes caps the in-memory recent history retained for
	// console tail-on-connect. 8 KB easily holds 20 lines at typical
	// serial widths.
	ringBufferBytes = 8 * 1024

	// consoleHistoryLines is the per-connect history tail delivered to
	// a fresh console subscriber (L18).
	consoleHistoryLines = 20

	// logDirMode is the permission bits used when ensuring the per-VM
	// log directory exists. Multiplexer state is owner-only.
	logDirMode = 0o700
)

// ErrConsoleInUse is returned by SubscribeConsole when another console
// subscriber is already attached to the same multiplexer. The agent
// console handler maps this to 409 console_in_use per ADR 0028.
var ErrConsoleInUse = errors.New("serialmux: console already in use")

// errMultiplexerClosed is returned by Reconnect when the multiplexer
// has already been Close'd. Callers (Manager.Reboot) treat this as a
// signal that the VM lifecycle has moved past reboot.
var errMultiplexerClosed = errors.New("serialmux: multiplexer closed")

// Multiplexer is the per-VM serial console multiplexer described by
// ADR 0029. One instance per running VM lives in the agent's Manager;
// callers attach a single bidirectional ConsoleSubscriber or any
// number of read-only LogsSubscribers.
//
// Lock order is fixed to avoid future maintainer foot-guns:
// transitionMu (Reconnect/Close serialisation) is taken first; subMu
// (subscriber registry) and the per-resource muxes (connMu, logMu)
// are always taken later and never while holding any other mux from
// this struct except transitionMu.
type Multiplexer struct {
	vmName string
	logDir string
	log    *slog.Logger
	dial   func() (net.Conn, error)

	rotationThreshold int64

	transitionMu sync.Mutex
	closed       atomic.Bool

	subMu      sync.Mutex
	consoleSub *ConsoleSubscriber
	logsSubs   []*LogsSubscriber

	connMu sync.Mutex
	conn   net.Conn

	ring *RingBuffer

	logMu        sync.Mutex
	logFile      *os.File
	bytesWritten atomic.Int64

	done chan struct{}

	rotationCh chan struct{}
	pumpWg     sync.WaitGroup
	bgWg       sync.WaitGroup
}

// New constructs a multiplexer that dials qemuSocket as a Unix-domain
// socket. Per L13 a dial failure is fatal - Manager.Start surfaces it
// instead of declaring a half-running VM.
func New(vmName, qemuSocket, logDir string, log *slog.Logger) (*Multiplexer, error) {
	return newMux(vmName, logDir, defaultDial(qemuSocket), log)
}

func defaultDial(qemuSocket string) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		return net.Dial("unix", qemuSocket)
	}
}

// newMux is the test-friendly constructor that accepts a connection
// factory instead of a hard-coded socket path. New delegates here
// after wrapping qemuSocket in defaultDial.
func newMux(vmName, logDir string, dial func() (net.Conn, error), log *slog.Logger) (*Multiplexer, error) {
	return newMuxWithThreshold(vmName, logDir, dial, log, rotationThresholdBytes)
}

// newMuxWithThreshold lets tests shrink the rotation threshold so the
// rotation cycle is exercisable without writing 10 MB to disk.
func newMuxWithThreshold(vmName, logDir string, dial func() (net.Conn, error), log *slog.Logger, threshold int64) (*Multiplexer, error) {
	if vmName == "" {
		return nil, errors.New("serialmux: vm name is required")
	}
	if logDir == "" {
		return nil, errors.New("serialmux: log dir is required")
	}
	if dial == nil {
		return nil, errors.New("serialmux: dial factory is required")
	}
	if log == nil {
		log = slog.Default()
	}
	if threshold <= 0 {
		threshold = rotationThresholdBytes
	}

	if err := os.MkdirAll(logDir, logDirMode); err != nil {
		return nil, fmt.Errorf("mkdir log dir %s: %v", logDir, err)
	}

	conn, err := dial()
	if err != nil {
		return nil, fmt.Errorf("dial qemu socket: %v", err)
	}

	logPath := filepath.Join(logDir, logFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, logFileMode) //nolint:gosec // logPath is logDir + fixed file name; logDir is agent-controlled.
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open log file %s: %v", logPath, err)
	}
	stat, err := logFile.Stat()
	if err != nil {
		_ = logFile.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("stat log file %s: %v", logPath, err)
	}

	m := &Multiplexer{
		vmName:            vmName,
		logDir:            logDir,
		log:               log.With("component", "serialmux", "vm", vmName),
		dial:              dial,
		rotationThreshold: threshold,
		conn:              conn,
		ring:              NewRingBuffer(ringBufferBytes),
		logFile:           logFile,
		done:              make(chan struct{}),
		rotationCh:        make(chan struct{}, 1),
	}
	m.bytesWritten.Store(stat.Size())

	m.bgWg.Add(1)
	go m.rotationLoop()
	m.spawnPump()
	return m, nil
}

// SubscribeConsole attaches a console subscriber. Returns
// ErrConsoleInUse if another live console subscriber is already
// attached.
//
// On success the subscriber receives up to consoleHistoryLines of
// recent buffered output followed by a visual separator before any
// live bytes, per L18.
func (m *Multiplexer) SubscribeConsole() (*ConsoleSubscriber, error) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	if m.consoleSub != nil && !m.consoleSub.IsClosed() {
		return nil, ErrConsoleInUse
	}

	sub := newConsoleSubscriber(m.writeToQEMU, defaultSubscriberCapacity)
	m.consoleSub = sub

	history := m.ring.LastNLines(consoleHistoryLines)
	if len(history) > 0 {
		_ = sub.tryDeliver(history)
		_ = sub.tryDeliver([]byte("\n[otherix: --- live console ---]\n"))
	}
	return sub, nil
}

// SubscribeLogs attaches a read-only logs subscriber. tailLines
// follows kubectl semantics: -1 (or any negative value) yields the
// full history, 0 yields no history, positive N yields the trailing
// N lines. follow controls whether the subscriber remains attached
// for future bytes; when false the subscriber is Done() once the
// initial tail has been delivered.
//
// History is streamed asynchronously - the call returns immediately
// while a background goroutine reads the disk and attaches the
// subscriber to the live fan-out.
func (m *Multiplexer) SubscribeLogs(tailLines int, follow bool) *LogsSubscriber {
	sub := newLogsSubscriber(follow, defaultSubscriberCapacity)
	m.bgWg.Add(1)
	go m.streamHistoryThenAttach(sub, tailLines)
	return sub
}

// Reconnect closes the active QEMU connection and dials again. Used
// by Manager.Reboot - the QEMU process is recycled but log file,
// subscribers, and ring buffer continuity must survive (L15).
//
// Reconnect is serialised against Close via transitionMu; a Reconnect
// invoked after Close returns errMultiplexerClosed.
func (m *Multiplexer) Reconnect() error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	if m.closed.Load() {
		return errMultiplexerClosed
	}

	m.connMu.Lock()
	oldConn := m.conn
	m.conn = nil
	m.connMu.Unlock()
	if oldConn != nil {
		_ = oldConn.Close()
	}
	// Drain the old pump before installing a new connection so two
	// pumps never compete on the conn pointer.
	m.pumpWg.Wait()

	newConn, err := m.dial()
	if err != nil {
		return fmt.Errorf("reconnect: %v", err)
	}
	m.connMu.Lock()
	m.conn = newConn
	m.connMu.Unlock()
	m.spawnPump()
	return nil
}

// Close tears down the multiplexer. Idempotent: subsequent calls are
// no-ops. The QEMU connection is closed (unblocking the pump), the
// log file is flushed and closed, and every attached subscriber's
// Done() channel fires.
//
// Close holds transitionMu for its entire run so a concurrent
// Reconnect cannot stream a fresh connection into the half-torn-down
// state.
func (m *Multiplexer) Close() error {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	if m.closed.Swap(true) {
		return nil
	}
	close(m.done)

	m.connMu.Lock()
	oldConn := m.conn
	m.conn = nil
	m.connMu.Unlock()
	if oldConn != nil {
		_ = oldConn.Close()
	}
	m.pumpWg.Wait()
	// Drain rotation goroutine + any in-flight history streams.
	m.bgWg.Wait()

	m.logMu.Lock()
	if m.logFile != nil {
		_ = m.logFile.Sync()
		_ = m.logFile.Close()
		m.logFile = nil
	}
	m.logMu.Unlock()

	m.subMu.Lock()
	if m.consoleSub != nil {
		_ = m.consoleSub.Close()
		m.consoleSub = nil
	}
	for _, sub := range m.logsSubs {
		_ = sub.Close()
	}
	m.logsSubs = nil
	m.subMu.Unlock()
	return nil
}

func (m *Multiplexer) spawnPump() {
	m.pumpWg.Add(1)
	go m.pump()
}

func (m *Multiplexer) pump() {
	defer m.pumpWg.Done()
	buf := make([]byte, pumpBufferSize)
	for {
		m.connMu.Lock()
		conn := m.conn
		m.connMu.Unlock()
		if conn == nil {
			return
		}
		n, err := conn.Read(buf)
		if n > 0 {
			clean := sanitize(buf[:n])
			if len(clean) > 0 {
				m.ring.Write(clean)
				m.writeToDisk(clean)
				m.deliverToSubscribers(clean)
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *Multiplexer) writeToDisk(data []byte) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	if m.logFile == nil {
		return
	}
	if _, err := m.logFile.Write(data); err != nil {
		m.log.Warn("log file write failed", "error", err.Error())
		return
	}
	if n := m.bytesWritten.Add(int64(len(data))); n >= m.rotationThreshold {
		select {
		case m.rotationCh <- struct{}{}:
		default:
		}
	}
}

func (m *Multiplexer) rotationLoop() {
	defer m.bgWg.Done()
	for {
		select {
		case <-m.done:
			return
		case <-m.rotationCh:
			m.runRotation()
		}
	}
}

func (m *Multiplexer) runRotation() {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	if m.logFile == nil {
		return
	}
	if err := m.logFile.Sync(); err != nil {
		m.log.Warn("pre-rotate sync failed", "error", err.Error())
	}
	if err := m.logFile.Close(); err != nil {
		m.log.Warn("pre-rotate close failed", "error", err.Error())
	}
	m.logFile = nil
	newFile, err := rotateLogFile(m.logDir)
	if err != nil {
		m.log.Error("rotation failed", "error", err.Error())
		// Best-effort: reopen the current path so subsequent writes
		// don't vanish silently.
		path := filepath.Join(m.logDir, logFileName)
		recovered, err2 := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, logFileMode) //nolint:gosec // see open in newMuxWithThreshold.
		if err2 != nil {
			m.log.Error("post-rotation reopen failed", "error", err2.Error())
			return
		}
		m.logFile = recovered
		// Reset the byte counter even on failure: leaving it at >=
		// threshold would make every subsequent pump write re-enqueue
		// rotation, hot-looping the rotation goroutine. The on-disk
		// file may still exceed the spec cap, but we recover the next
		// time bytesWritten climbs another threshold's worth.
		m.bytesWritten.Store(0)
		return
	}
	m.logFile = newFile
	m.bytesWritten.Store(0)
}

func (m *Multiplexer) deliverToSubscribers(data []byte) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	if m.consoleSub != nil && !m.consoleSub.IsClosed() {
		// Console subscriber gets a private copy so the slice is not
		// reused under the consumer's feet.
		_ = m.consoleSub.tryDeliver(cloneBytes(data))
	}

	alive := m.logsSubs[:0]
	for _, sub := range m.logsSubs {
		if sub.IsClosed() {
			continue
		}
		payload := cloneBytes(data)
		// Atomically consume any pending drop count and prepend the
		// marker to this payload, so a single channel send delivers
		// both at once. Without prepending, a slow consumer whose
		// channel has exactly one free slot would always re-fill
		// that slot with the payload itself, leaving no room for a
		// follow-up marker delivery and stranding the count forever.
		drops := sub.SwapDropped()
		toSend := payload
		if drops > 0 {
			marker := fmt.Appendf(nil, "\n[otherix: %d bytes dropped]\n", drops)
			toSend = make([]byte, 0, len(marker)+len(payload))
			toSend = append(toSend, marker...)
			toSend = append(toSend, payload...)
		}
		if err := sub.tryDeliver(toSend); err != nil {
			if errors.Is(err, errSubscriberFull) {
				// Restore the consumed drops plus this newly
				// dropped payload so a future delivery can flush
				// the accurate total.
				sub.AddDropped(drops + uint64(len(payload)))
			}
		}
		alive = append(alive, sub)
	}
	m.logsSubs = alive
}

func (m *Multiplexer) writeToQEMU(data []byte) error {
	m.connMu.Lock()
	conn := m.conn
	m.connMu.Unlock()
	if conn == nil {
		return errors.New("serialmux: connection unavailable")
	}
	_, err := conn.Write(data)
	return err
}

func (m *Multiplexer) streamHistoryThenAttach(sub *LogsSubscriber, tailLines int) {
	defer m.bgWg.Done()
	defer sub.markReady()

	var tail []byte
	if tailLines != 0 {
		t, err := readTailLines(m.logDir, tailLines)
		if err != nil {
			m.log.Error("read tail failed", "error", err.Error())
		} else {
			tail = t
		}
	}
	if !sub.Follow() {
		if len(tail) > 0 {
			_ = sub.tryDeliver(tail)
		}
		_ = sub.Close()
		return
	}
	// Atomically deliver the history tail and append the subscriber
	// under subMu. Holding the lock around both calls guarantees the
	// pump's fan-out cannot interleave a live byte between the
	// history payload and the subscriber's first live delivery. The
	// brief block on the pump is bounded by the time of one
	// non-blocking channel send.
	m.subMu.Lock()
	if len(tail) > 0 {
		_ = sub.tryDeliver(tail)
	}
	m.logsSubs = append(m.logsSubs, sub)
	m.subMu.Unlock()
}

// subscribeLogsWithCapacity exposes the subscriber channel capacity
// to tests so they can drive the slow-consumer drop-accounting path
// without saturating the default 32-slot channel through bursts.
func (m *Multiplexer) subscribeLogsWithCapacity(tailLines int, follow bool, capacity int) *LogsSubscriber {
	sub := newLogsSubscriber(follow, capacity)
	m.bgWg.Add(1)
	go m.streamHistoryThenAttach(sub, tailLines)
	return sub
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
