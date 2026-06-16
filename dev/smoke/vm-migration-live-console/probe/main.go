// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command probe is the headless console client for the
// vm-migration-live-console smoke. It opens ONE interactive console
// WebSocket session against the CP (the same console-token + proxy-WS
// data path the `otherix vm console` operator command uses, minus the
// raw-tty front end that needs a real terminal), types a PRE marker into
// the guest shell, signals run.sh (the ready file), waits for run.sh to
// live-migrate the VM and signal back (the go file), then types a POST
// marker on the SAME WebSocket and asserts its echo returns. A surviving
// echo after the cutover proves the console session followed the VM
// across the live migration.
//
// It takes the CP URL + API token as flags (run.sh extracts them from
// the operator's ~/.otherix/config) and speaks the REST console-token +
// proxy-WS contract directly, so it stays decoupled from the CLI's
// internal packages while exercising the same data path.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

func main() {
	cfg := parseFlags()
	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, "probe: PRE and POST markers both echoed on the same console session")
}

type config struct {
	cpURL, token, vm    string
	readyOut, goIn      string
	pre, post           string
	settle, markTimeout time.Duration
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.cpURL, "cp-url", "http://localhost:8080", "CP base URL")
	flag.StringVar(&c.token, "token", "", "API token (otx_...) (required)")
	flag.StringVar(&c.vm, "vm", "", "VM name to open a console against (required)")
	flag.StringVar(&c.readyOut, "ready", "", "file to touch once the PRE marker echoes (required)")
	flag.StringVar(&c.goIn, "go", "", "file run.sh touches after the migration to release the POST marker (required)")
	flag.StringVar(&c.pre, "marker-pre", "PRECONSOLE", "PRE marker token")
	flag.StringVar(&c.post, "marker-post", "POSTCONSOLE", "POST marker token")
	flag.DurationVar(&c.settle, "settle", 6*time.Second, "time to let the guest console settle before the PRE marker")
	flag.DurationVar(&c.markTimeout, "timeout", 120*time.Second, "deadline for each marker phase")
	flag.Parse()
	if c.token == "" || c.vm == "" || c.readyOut == "" || c.goIn == "" {
		fmt.Fprintln(os.Stderr, "probe: --token, --vm, --ready and --go are required")
		os.Exit(2)
	}
	return c
}

func run(ctx context.Context, c config) error {
	wsURL, err := issueConsole(ctx, c)
	if err != nil {
		return fmt.Errorf("issue console ticket: %v", err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial console WS: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	sink := newSink()
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	go func() {
		for {
			_, data, rerr := conn.Read(readerCtx)
			if rerr != nil {
				return
			}
			sink.append(data)
		}
	}()

	// Let boot / cloud-init console noise settle, nudge a fresh prompt.
	time.Sleep(c.settle)
	if err := typeLine(ctx, conn, ""); err != nil {
		return fmt.Errorf("prime newline: %v", err)
	}

	// PRE marker: type `echo <pre>` and wait for the token to echo back
	// (the autologin shell echoes the typed command and its output, so
	// seeing the token proves the operator->guest->operator round trip).
	if err := sendUntilEcho(ctx, conn, sink, c.pre, c.markTimeout); err != nil {
		return fmt.Errorf("PRE marker never echoed (console not interactive before migration): %v\n--- recv tail ---\n%s", err, sink.tail(800))
	}
	if err := touch(c.readyOut); err != nil {
		return fmt.Errorf("touch ready file: %v", err)
	}

	// Wait for run.sh to finish the live migration and release us.
	if err := waitForFile(ctx, c.goIn, 10*time.Minute); err != nil {
		return fmt.Errorf("waiting for go signal: %v", err)
	}

	// POST marker on the SAME WebSocket. If the session followed the VM,
	// the CP re-attached to the target agent (re-minting a token) and this
	// echoes back; if it did not, the WS is dead and this times out.
	if err := sendUntilEcho(ctx, conn, sink, c.post, c.markTimeout); err != nil {
		return fmt.Errorf("POST marker never echoed (console did NOT follow the VM across migration): %v\n--- recv tail ---\n%s", err, sink.tail(1200))
	}
	return nil
}

// issueConsole POSTs the console-token request and returns the proxy WS
// URL (which already carries the single-use token in its query).
func issueConsole(ctx context.Context, c config) (string, error) {
	body := bytes.NewBufferString(`{"protocol":"serial"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.cpURL, "/")+"/v1/vms/"+c.vm+"/console", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("console-token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		WebsocketURL string `json:"websocket_url"`
		Token        string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode ticket: %v", err)
	}
	if out.WebsocketURL == "" {
		return "", fmt.Errorf("empty websocket_url in ticket: %s", string(raw))
	}
	return out.WebsocketURL, nil
}

// sendUntilEcho retypes `echo <token>` every 1.5s until <token> appears in
// the received stream, or the deadline elapses. Retyping is safe
// (idempotent on a shell) and rides over a brief cutover micro-pause where
// the first keystrokes are buffered by the CP and replayed on re-attach.
func sendUntilEcho(ctx context.Context, conn *websocket.Conn, s *sink, token string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := typeLine(ctx, conn, "echo "+token); err != nil {
			return err
		}
		inner := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(inner) {
			if strings.Contains(s.string(), token) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return fmt.Errorf("token %q not seen within %s", token, timeout)
}

// typeLine writes line + CR to the console as a binary WS frame, the way a
// terminal sends a keystroke run followed by Enter.
func typeLine(ctx context.Context, conn *websocket.Conn, line string) error {
	return conn.Write(ctx, websocket.MessageBinary, []byte(line+"\r"))
}

type sink struct {
	mu  sync.Mutex
	buf []byte
}

func newSink() *sink { return &sink{} }

func (s *sink) append(b []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, b...)
	s.mu.Unlock()
}

func (s *sink) string() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

func (s *sink) tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) <= n {
		return string(s.buf)
	}
	return "..." + string(s.buf[len(s.buf)-n:])
}

func touch(path string) error {
	f, err := os.Create(path) //nolint:gosec // smoke-controlled path
	if err != nil {
		return err
	}
	return f.Close()
}

func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("file %s did not appear within %s", path, timeout)
}
