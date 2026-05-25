// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// escapeByte is the telnet-style escape character the CLI reserves
// to bring up the local "close console?" prompt. Ctrl+] = 0x1D.
// Operators that need to send 0x1D to the guest must double-tap (one
// to invoke the prompt, decline, then re-send) or rely on a future
// "send literal byte" prompt command.
const escapeByte byte = 0x1D

// stdinBufSize bounds a single read of operator keystrokes. Picked
// keyboard-friendly — bigger than typical paste blocks without burning
// memory.
const stdinBufSize = 4096

// newConsoleCommand returns `otherix vm console <vm-name>`. The
// subcommand drops the operator into the VM's serial console via a
// WebSocket session opened against the CP's `vms.console` ticket.
// Terminal goes to raw mode for the duration; `Ctrl+]` brings up a
// local prompt (yes/no) to close the session.
func newConsoleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "console <vm-name>",
		Short: "Attach to a VM's serial console (interactive).",
		Long: `Opens an interactive serial console session against the named VM
through the Control Plane's console-ticket flow.

The VM must be running. Only the serial protocol is implemented;
vnc / spice are reserved for future iterations.

Escape sequence:
  Ctrl+]    bring up a local "close console? (y/N)" prompt. Answering
            'y' detaches; any other key resumes the session.

The CLI never sends Ctrl+] (0x1D) to the guest. Operators that need
to pass that byte literally must use a different tool (this is the
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
		return fmt.Errorf("stdin is not a terminal — console requires an interactive shell")
	}

	// Dial BEFORE flipping the terminal to raw mode so any failure
	// surfaces against a normally-rendered stderr (no stale "connected
	// to ..." banner above the error). Likewise we inspect the upstream
	// response on dial failure and translate well-known HTTP statuses
	// into operator-language instead of leaking the WebSocket internals.
	wsConn, dialResp, err := websocket.Dial(cmd.Context(), resp.WebsocketURL, &websocket.DialOptions{
		HTTPClient: c.HTTPClient(),
	})
	if err != nil {
		return classifyConsoleDialError(vmName, dialResp, err)
	}
	defer func() { _ = wsConn.Close(websocket.StatusInternalError, "") }()

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		_ = wsConn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("enter raw mode: %v", err)
	}
	restore := newTerminalRestorer(fd, oldState)
	defer restore()

	stderr := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(stderr, "\r\nconnected to %s (serial console). Press Ctrl+] to detach.\r\n", vmName)

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

// classifyConsoleDialError turns a coder/websocket.Dial failure into an
// operator-facing error. websocket.Dial returns the upstream HTTP
// response when the upgrade was rejected (non-101 status); the CP and
// the agent both populate a JSON error envelope on those 4xx / 5xx
// paths, so the most-actionable message is the envelope's `message`
// field. When the envelope is missing (network failure mid-handshake
// etc.) we fall back to a generic message keyed off the HTTP status
// so the operator never has to read about WebSocket frame negotiation.
func classifyConsoleDialError(vmName string, resp *http.Response, dialErr error) error {
	if resp == nil {
		return fmt.Errorf("connect_failed: %s: %v", vmName, dialErr)
	}
	envelope := decodeErrorEnvelope(resp)
	switch resp.StatusCode {
	case http.StatusConflict:
		switch envelope.Code {
		case "console_in_use":
			return fmt.Errorf("console_in_use: another console session is already open for %s; close it (Ctrl+] in the other terminal) and retry", vmName)
		case "vm_not_running":
			return fmt.Errorf("vm_not_running: %s is not running; start it first ('otherix vm start %s')", vmName, vmName)
		case "protocol_not_available":
			return fmt.Errorf("protocol_not_available: serial console is not exposed by %s", vmName)
		}
		return fmt.Errorf("conflict: %s: %s", vmName, envelope.Message)
	case http.StatusUnauthorized:
		return fmt.Errorf("unauthenticated: %s rejected the console token (likely expired or already consumed); retry the command", vmName)
	case http.StatusForbidden:
		return fmt.Errorf("permission_denied: you do not have permission to open a console on %s", vmName)
	case http.StatusNotFound:
		return fmt.Errorf("vm_not_found: %s", vmName)
	case http.StatusBadGateway:
		return fmt.Errorf("agent_unreachable: control plane could not reach the agent owning %s", vmName)
	}
	if envelope.Message != "" {
		return fmt.Errorf("%s: %s", strings.ToLower(envelope.Code), envelope.Message)
	}
	return fmt.Errorf("connect_failed: %s: status %d", vmName, resp.StatusCode)
}

// errorEnvelope mirrors the JSON shape control-plane.yaml's
// `components/schemas/Error` and the agent's matching shape. We keep
// it local instead of importing the response package so the CLI does
// not pull in the server-side envelope helpers.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// decodeErrorEnvelope reads up to a few KiB of the response body and
// extracts the inner error object. Missing / malformed payload yields
// a zero-value envelope so the caller can fall back to status-based
// classification.
func decodeErrorEnvelope(resp *http.Response) errorEnvelope {
	if resp.Body == nil {
		return errorEnvelope{}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil || len(body) == 0 {
		return errorEnvelope{}
	}
	var wire struct {
		Error errorEnvelope `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return errorEnvelope{}
	}
	return wire.Error
}

// newTerminalRestorer wraps term.Restore in a once-only closure so
// happy-path explicit calls and the deferred safety net cooperate
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

// pumpWebSocketToStdout copies binary frames received from the
// WebSocket to stdout. First failure cancels the shared context so
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

// pumpStdinToWebSocket copies operator keystrokes to the WebSocket with
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

// handleStdinChunk processes a single stdin read. Returns false to
// signal "stop the pump" (operator detached, write failed). The
// caller's loop continues when true.
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

// findEscape returns the index of the first escapeByte in data, or -1
// if absent. Operator presses Ctrl+] and we route around the
// WebSocket — sending it would deliver the byte to the guest, which
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
// the operator "close console? (y/N)", and returns true if they
// confirm. Any read error short-circuits to "close" (we cannot
// recover a usable terminal from a partial restore). The caller is
// responsible for re-entering raw mode when the operator declines.
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
