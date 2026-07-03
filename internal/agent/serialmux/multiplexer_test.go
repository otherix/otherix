// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// muxFixture builds a multiplexer attached to a net.Pipe pair so the
// test can write QEMU-side bytes through the client end and observe
// behaviour via the multiplexer surface.
type muxFixture struct {
	t       *testing.T
	mux     *Multiplexer
	clients []net.Conn // one per Dial invocation; test writes through these
	pending []net.Conn // future Dial returns these
	servers []net.Conn // the multiplexer's side; kept for forced close in tests
	dialErr error
	mu      sync.Mutex
	logDir  string
}

func newMuxFixture(t *testing.T, n int) *muxFixture {
	t.Helper()
	logDir := t.TempDir()
	f := &muxFixture{t: t, logDir: logDir}
	for range n {
		client, server := net.Pipe()
		f.clients = append(f.clients, client)
		f.pending = append(f.pending, server)
	}
	return f
}

func (f *muxFixture) dial() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	if len(f.pending) == 0 {
		return nil, errors.New("muxFixture: no more pre-allocated connections")
	}
	c := f.pending[0]
	f.pending = f.pending[1:]
	f.servers = append(f.servers, c)
	return c, nil
}

func (f *muxFixture) start() {
	f.t.Helper()
	mux, err := newMux("vm-test", f.logDir, f.dial, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatalf("newMux: %v", err)
	}
	f.mux = mux
	f.t.Cleanup(func() {
		_ = f.mux.Close()
		for _, c := range f.clients {
			_ = c.Close()
		}
	})
}

func (f *muxFixture) writeFromQEMU(idx int, data []byte) {
	f.t.Helper()
	if _, err := f.clients[idx].Write(data); err != nil {
		f.t.Fatalf("write to client[%d]: %v", idx, err)
	}
}

// readSubscriber drains the subscriber's channel until it accumulates
// >= want bytes or the timeout elapses.
func readSubscriber(t *testing.T, ch <-chan []byte, want int, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.After(timeout)
	var got []byte
	for len(got) < want {
		select {
		case b := <-ch:
			got = append(got, b...)
		case <-deadline:
			t.Fatalf("subscriber read timed out; got %d bytes (%q), want >= %d", len(got), got, want)
		}
	}
	return got
}

// waitForLogContent polls path until its contents contain wantSub or
// the timeout elapses. Used as a deterministic alternative to
// time.Sleep when verifying that the pump has landed bytes on disk.
func waitForLogContent(t *testing.T, path, wantSub string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(wantSub)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waitForLogContent(%q): timed out; last contents = %q (err=%v)", wantSub, b, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForCondition polls cond until it returns true or the timeout
// elapses. Generic poll helper for goroutine-completion checks.
func waitForCondition(t *testing.T, msg string, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waitForCondition(%s): timed out", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMultiplexer_NewFailsOnDialError(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 0)
	f.dialErr = errors.New("synthetic dial failure")
	_, err := newMux("vm-x", f.logDir, f.dial, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatalf("newMux: got nil error, want non-nil")
	}
}

func TestMultiplexer_NewCreatesLogFile(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()
	logPath := filepath.Join(f.logDir, logFileName)
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file not created: %v", err)
	}
}

func TestMultiplexer_PumpFansBytesToConsoleSubscriber(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	sub, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Fatalf("SubscribeConsole: %v", err)
	}
	defer func() { _ = sub.Close() }()

	f.writeFromQEMU(0, []byte("hello\nworld\n"))

	got := readSubscriber(t, sub.Bytes(), len("hello\nworld\n"), 2*time.Second)
	if !bytes.Contains(got, []byte("hello\nworld\n")) {
		t.Errorf("console subscriber got %q, want to contain %q", got, "hello\nworld\n")
	}
}

func TestMultiplexer_PumpWritesToDisk(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	f.writeFromQEMU(0, []byte("disk-line\n"))
	waitForLogContent(t, filepath.Join(f.logDir, logFileName), "disk-line\n", 2*time.Second)
}

func TestMultiplexer_ConsoleInUse(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	sub1, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Fatalf("first SubscribeConsole: %v", err)
	}
	defer func() { _ = sub1.Close() }()

	if _, err := f.mux.SubscribeConsole(); !errors.Is(err, ErrConsoleInUse) {
		t.Errorf("second SubscribeConsole: got %v, want ErrConsoleInUse", err)
	}
}

func TestMultiplexer_ConsoleReattachAfterClose(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	sub1, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Fatalf("first SubscribeConsole: %v", err)
	}
	_ = sub1.Close()

	sub2, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Errorf("re-subscribe after close: got %v, want nil", err)
	}
	if sub2 != nil {
		_ = sub2.Close()
	}
}

func TestMultiplexer_ConsoleHistoryDeliveredOnSubscribe(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	// Push history before subscribing so the ring buffer is primed.
	// Polling the disk file is a reliable proxy: the pump writes to
	// the ring and disk in the same statement.
	f.writeFromQEMU(0, []byte("line-1\nline-2\nline-3\n"))
	waitForLogContent(t, filepath.Join(f.logDir, logFileName), "line-3", 2*time.Second)

	sub, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Fatalf("SubscribeConsole: %v", err)
	}
	defer func() { _ = sub.Close() }()

	// Sample at least the ring-buffered seed length so we know history
	// landed without depending on the now-removed visual separator.
	got := readSubscriber(t, sub.Bytes(), len("line-1\nline-2\nline-3\n"), 2*time.Second)
	if !bytes.Contains(got, []byte("line-1")) {
		t.Errorf("first delivery should carry history, got %q", got)
	}
	if !bytes.Contains(got, []byte("line-3")) {
		t.Errorf("first delivery should carry history through to the tail, got %q", got)
	}
}

func TestMultiplexer_SubscribeLogsFollowDeliversFuture(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	sub := f.mux.SubscribeLogs(0, true)
	defer func() { _ = sub.Close() }()

	// Wait deterministically until the subscriber is attached for
	// live fan-out; otherwise the pump could deliver the next bytes
	// before append and the test would lose them.
	<-sub.Ready()

	f.writeFromQEMU(0, []byte("live-data\n"))
	got := readSubscriber(t, sub.Bytes(), len("live-data\n"), 2*time.Second)
	if !bytes.Contains(got, []byte("live-data\n")) {
		t.Errorf("logs subscriber got %q, want %q", got, "live-data\n")
	}
}

func TestMultiplexer_SubscribeLogsFollowFalseClosesAfterTail(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	// Seed the disk file so tail has content.
	logPath := filepath.Join(f.logDir, logFileName)
	if err := os.WriteFile(logPath, []byte("seed-1\nseed-2\n"), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	sub := f.mux.SubscribeLogs(-1, false)

	select {
	case <-sub.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("subscriber not Done() after tail without follow")
	}
}

func TestMultiplexer_SubscribeLogsTailHonoured(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	logPath := filepath.Join(f.logDir, logFileName)
	if err := os.WriteFile(logPath, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	sub := f.mux.SubscribeLogs(1, false)

	deadline := time.After(2 * time.Second)
	var got []byte
LOOP:
	for {
		select {
		case b := <-sub.Bytes():
			got = append(got, b...)
		case <-sub.Done():
			// Done can fire while the history tail is still buffered in
			// Bytes(): streamHistoryThenAttach delivers the tail and then
			// closes, so both channels are ready and the outer select may
			// pick Done first. Drain the buffered bytes before stopping,
			// mirroring the production streamLogs drainSubscriber path.
			for {
				select {
				case b := <-sub.Bytes():
					got = append(got, b...)
				default:
					break LOOP
				}
			}
		case <-deadline:
			t.Fatalf("subscriber stalled with got=%q", got)
		}
	}
	if string(got) != "c\n" {
		t.Errorf("logs tail=1 delivered %q, want %q", got, "c\n")
	}
}

func TestMultiplexer_CloseDetachesSubscribers(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	cs, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Fatalf("SubscribeConsole: %v", err)
	}
	ls := f.mux.SubscribeLogs(0, true)

	if err := f.mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Console + logs subscriber Done() must fire.
	select {
	case <-cs.Done():
	case <-time.After(2 * time.Second):
		t.Errorf("console subscriber Done not signalled after Close")
	}
	select {
	case <-ls.Done():
	case <-time.After(2 * time.Second):
		t.Errorf("logs subscriber Done not signalled after Close")
	}
}

func TestMultiplexer_ReconnectSwitchesConnection(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 2)
	f.start()

	sub, err := f.mux.SubscribeConsole()
	if err != nil {
		t.Fatalf("SubscribeConsole: %v", err)
	}
	defer func() { _ = sub.Close() }()

	f.writeFromQEMU(0, []byte("from-conn-1\n"))
	got := readSubscriber(t, sub.Bytes(), len("from-conn-1\n"), 2*time.Second)
	if !bytes.Contains(got, []byte("from-conn-1\n")) {
		t.Fatalf("pre-reconnect missed bytes; got %q", got)
	}

	if err := f.mux.Reconnect(); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	f.writeFromQEMU(1, []byte("from-conn-2\n"))
	got2 := readSubscriber(t, sub.Bytes(), len("from-conn-2\n"), 2*time.Second)
	if !bytes.Contains(got2, []byte("from-conn-2\n")) {
		t.Errorf("post-reconnect missed bytes; got %q", got2)
	}
}

func TestMultiplexer_RotationFiresAtThreshold(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	logDir := f.logDir
	mux, err := newMuxWithThreshold("vm-test", logDir, f.dial, slog.New(slog.NewTextHandler(io.Discard, nil)), 16)
	if err != nil {
		t.Fatalf("newMuxWithThreshold: %v", err)
	}
	f.mux = mux
	t.Cleanup(func() {
		_ = mux.Close()
		for _, c := range f.clients {
			_ = c.Close()
		}
	})

	f.writeFromQEMU(0, []byte(strings.Repeat("X", 32)))

	rotatedPath := filepath.Join(logDir, rotatedFileName)
	waitForCondition(t, "rotation produces serial.log.1", func() bool {
		_, err := os.Stat(rotatedPath)
		return err == nil
	}, 2*time.Second)
}

func TestMultiplexer_DropMarkerInjectedAfterRecovery(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	// Capacity 1 so the very next pump delivery after the first hit
	// overflows the channel and feeds the drop counter.
	sub := f.mux.subscribeLogsWithCapacity(0, true, 1)
	defer func() { _ = sub.Close() }()
	<-sub.Ready()

	// Send three bursts; the subscriber's channel holds one slot, so
	// the second and third pump deliveries will overflow.
	f.writeFromQEMU(0, []byte("a"))
	f.writeFromQEMU(0, []byte("b"))
	f.writeFromQEMU(0, []byte("c"))

	// Wait for the drop counter to reflect at least one dropped chunk.
	waitForCondition(t, "drop counter increments under overflow", func() bool {
		return sub.DroppedBytes() > 0
	}, 2*time.Second)

	// Drain the queued chunk; this frees the capacity slot.
	select {
	case <-sub.Bytes():
	case <-time.After(2 * time.Second):
		t.Fatalf("could not drain pending chunk before recovery write")
	}

	// Push a recovery byte; the next pump delivery should succeed and
	// then emit the drop marker the spec defines.
	f.writeFromQEMU(0, []byte("z"))

	deadline := time.After(3 * time.Second)
	var got []byte
	for !bytes.Contains(got, []byte("bytes dropped")) {
		select {
		case b := <-sub.Bytes():
			got = append(got, b...)
		case <-deadline:
			t.Fatalf("drop marker never appeared; got = %q", got)
		}
	}
	if !bytes.Contains(got, []byte("[otherix: ")) {
		t.Errorf("drop marker prefix missing; got = %q", got)
	}
}

func TestMultiplexer_ReconnectAfterCloseReturnsError(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 2)
	f.start()

	if err := f.mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.mux.Reconnect(); !errors.Is(err, errMultiplexerClosed) {
		t.Errorf("Reconnect after Close: got %v, want errMultiplexerClosed", err)
	}
}

func TestMultiplexer_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newMuxFixture(t, 1)
	f.start()

	if err := f.mux.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := f.mux.Close(); err != nil {
		t.Errorf("second Close: got %v, want nil", err)
	}
}
