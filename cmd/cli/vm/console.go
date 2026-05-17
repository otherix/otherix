// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// escapeByte is the telnet-style escape character the CLI reserves
// to bring up the local "close console?" prompt. Ctrl+] = 0x1D.
// Operators that need к send 0x1D to the guest must double-tap (one
// к invoke the prompt, decline, then re-send) or rely on а future
// "send literal byte" prompt command.
const escapeByte byte = 0x1D

// stdinBufSize bounds а single read of operator keystrokes. Picked
// keyboard-friendly — bigger than typical paste blocks без burning
// memory.
const stdinBufSize = 4096

// newConsoleCommand returns `otherix vm console <vm-name>`. The
// subcommand drops the operator into the VM's serial console via а
// WebSocket session opened against the CP's `vms.console` ticket.
// Terminal goes к raw mode for the duration; `Ctrl+]` brings up а
// local prompt (yes/no) to close the session.
func newConsoleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "console <vm-name>",
		Short: "Attach to а VM's serial console (interactive).",
		Long: `Opens an interactive serial console session against the named VM
through the Control Plane's console-ticket flow.

The VM must be running. Only the serial protocol is implemented;
vnc / spice are reserved for future iterations.

Escape sequence:
  Ctrl+]    bring up а local "close console? (y/N)" prompt. Answering
            'y' detaches; any other key resumes the session.

The CLI never sends Ctrl+] (0x1D) to the guest. Operators что need
к pass that byte literally must use а different tool (this is the
same trade-off ` + "`virsh console`" + ` and similar tools accept).`,
		Args: cobra.ExactArgs(1),
		RunE: runConsole,
	}
	return cmd
}

func runConsole(cmd *cobra.Command, args []string) error {
	vmName := args[0]
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}

	resp, err := c.VMConsole(cmd.Context(), vmName, "serial")
	if err != nil {
		return classifyError(err)
	}
	if resp.WebsocketURL == "" || resp.Token == "" {
		return fmt.Errorf("malformed console response (empty url or token)")
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not а terminal — console requires an interactive shell")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("enter raw mode: %v", err)
	}
	restore := newTerminalRestorer(fd, oldState)
	defer restore()

	stderr := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(stderr, "\r\nconnected к %s (serial console). Press Ctrl+] to detach.\r\n", vmName)

	wsConn, _, err := websocket.Dial(cmd.Context(), resp.WebsocketURL, &websocket.DialOptions{
		HTTPClient: c.HTTPClient(),
	})
	if err != nil {
		return fmt.Errorf("websocket dial: %v", err)
	}
	defer func() { _ = wsConn.Close(websocket.StatusInternalError, "") }()

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go pumpWebSocketToStdout(ctx, &wg, cancel, wsConn)
	go pumpStdinToWebSocket(ctx, &wg, cancel, wsConn, fd, oldState, restore, stderr)

	wg.Wait()
	_ = wsConn.Close(websocket.StatusNormalClosure, "")
	restore()
	_, _ = fmt.Fprintln(stderr, "session ended.")
	return nil
}

// newTerminalRestorer wraps term.Restore in а once-only closure so
// happy-path explicit calls и the deferred safety net cooperate
// without restoring twice.
func newTerminalRestorer(fd int, oldState *term.State) func() {
	restored := false
	return func() {
		if !restored {
			_ = term.Restore(fd, oldState)
			restored = true
		}
	}
}

// pumpWebSocketToStdout copies binary frames received от the
// WebSocket к stdout. First failure cancels the shared context so
// the stdin pump unwinds promptly.
func pumpWebSocketToStdout(ctx context.Context, wg *sync.WaitGroup, cancel context.CancelFunc, wsConn *websocket.Conn) {
	defer wg.Done()
	defer cancel()
	for {
		_, data, err := wsConn.Read(ctx)
		if err != nil {
			return
		}
		if _, werr := os.Stdout.Write(data); werr != nil {
			return
		}
	}
}

// pumpStdinToWebSocket copies operator keystrokes к the WebSocket с
// inline Ctrl+] escape detection. When the escape byte arrives,
// anything in the same read buffer ahead of it is flushed first
// (so half-typed input still reaches the guest), then the
// close-confirm prompt runs in restored terminal mode.
func pumpStdinToWebSocket(ctx context.Context, wg *sync.WaitGroup, cancel context.CancelFunc, wsConn *websocket.Conn, fd int, oldState *term.State, restore func(), stderr io.Writer) {
	defer wg.Done()
	defer cancel()
	buf := make([]byte, stdinBufSize)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if !handleStdinChunk(ctx, wsConn, buf[:n], fd, oldState, restore, stderr) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// handleStdinChunk processes а single stdin read. Returns false к
// signal "stop the pump" (operator detached, write failed). The
// caller's loop continues когда true.
func handleStdinChunk(ctx context.Context, wsConn *websocket.Conn, data []byte, fd int, oldState *term.State, restore func(), stderr io.Writer) bool {
	idx := findEscape(data)
	if idx < 0 {
		return wsConn.Write(ctx, websocket.MessageBinary, data) == nil
	}
	if idx > 0 {
		if werr := wsConn.Write(ctx, websocket.MessageBinary, data[:idx]); werr != nil {
			return false
		}
	}
	closeIt, perr := promptCloseConsole(fd, oldState, stderr)
	if perr != nil {
		return false
	}
	if closeIt {
		restore()
		_, _ = fmt.Fprintln(stderr, "console detached.")
		return false
	}
	return true
}

// findEscape returns the index of the first escapeByte в data, or -1
// if absent. Operator presses Ctrl+] и we route around the
// WebSocket — sending it would deliver the byte к the guest, which
// is the opposite of what the operator means with that keystroke.
func findEscape(data []byte) int {
	for i, b := range data {
		if b == escapeByte {
			return i
		}
	}
	return -1
}

// promptCloseConsole temporarily restores cooked terminal mode, asks
// the operator "close console? (y/N)", и returns true if they
// confirm. Any read error short-circuits к "close" (we cannot
// recover а usable terminal от а partial restore). The caller is
// responsible для re-entering raw mode когда the operator declines.
func promptCloseConsole(fd int, oldState *term.State, stderr io.Writer) (bool, error) {
	if err := term.Restore(fd, oldState); err != nil {
		return true, fmt.Errorf("restore terminal: %v", err)
	}
	_, _ = fmt.Fprint(stderr, "\r\nclose console? (y/N): ")
	var answer [1]byte
	n, err := os.Stdin.Read(answer[:])
	_, _ = fmt.Fprintln(stderr)
	if err != nil && !errors.Is(err, io.EOF) {
		return true, err
	}
	if _, rerr := term.MakeRaw(fd); rerr != nil {
		return true, fmt.Errorf("re-enter raw mode: %v", rerr)
	}
	if n == 0 {
		return false, nil
	}
	switch answer[0] {
	case 'y', 'Y':
		return true, nil
	}
	return false, nil
}
