// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// vmConsoleResponse mirrors VMConsoleResponse в the CP OpenAPI spec.
// Locally declared so the integration package doesn't import the CLI
// cpclient view (keeps test/code coupling thin).
type vmConsoleResponse struct {
	Token        string `json:"token"`
	WebsocketURL string `json:"websocket_url"`
	Protocol     string `json:"protocol"`
	ExpiresAt    string `json:"expires_at"`
}

// postConsoleRequest issues `POST /v1/vms/{name}/console` с the
// admin token и returns raw status / body. Helper mirrors the shape
// of postVMLifecycleRequest.
func (v *verticalSlice) postConsoleRequest(t *testing.T, ctx context.Context, vmID uuid.UUID, protocol, token string) (int, []byte) {
	t.Helper()
	row, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("resolve vm name for %s: %v", vmID, err)
	}
	body := map[string]string{"protocol": protocol}
	raw, _ := json.Marshal(body)
	url := v.cpServer.URL + "/v1/vms/" + row.Name + "/console"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new console request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token == "" {
		token = v.adminToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("console POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, bodyBytes
}

// TestVMConsole_HappyPath_ProxyMode drives the full proxy-mode flow
// end-to-end: create VM, transition к running, issue console token,
// open WebSocket via the CP relay, exchange а frame с the mock-agent,
// verify echo. Locks the proxy mode plumbing including the CP→agent
// WebSocket dial и bidirectional binary pump.
func TestVMConsole_HappyPath_ProxyMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-console", 0xc0, "private")

	vmName := "console-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Console requires `vm_runtime.phase = running`. vm.create lands
	// the row at phase=running per the mock fixture; just confirm.
	rt, err := v.store.Queries().GetVMRuntime(ctx, vmID)
	if err != nil {
		t.Fatalf("GetVMRuntime: %v", err)
	}
	if string(rt.Phase) != "running" {
		t.Fatalf("vm_runtime.phase = %q, want running", rt.Phase)
	}

	status, body := v.postConsoleRequest(t, ctx, vmID, "serial", "")
	if status != http.StatusOK {
		t.Fatalf("console POST status = %d, body = %s, want 200", status, body)
	}
	var resp vmConsoleResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode console response: %v", err)
	}
	if resp.Token == "" || resp.WebsocketURL == "" {
		t.Fatalf("console response missing token/url: %+v", resp)
	}
	if resp.Protocol != "serial" {
		t.Errorf("Protocol = %q, want serial", resp.Protocol)
	}
	// Proxy mode → URL host should be the CP, not the mock-agent.
	// httptest CP listens on plain HTTP, так что handler emits ws://
	// (scheme tracks the incoming request — r.TLS=nil here).
	if !strings.HasPrefix(resp.WebsocketURL, "ws://") {
		t.Errorf("WebsocketURL %q lacks ws:// prefix (proxy mode tracks request scheme)", resp.WebsocketURL)
	}
	if !strings.Contains(resp.WebsocketURL, "/v1/vms/"+vmName+"/console-stream") {
		t.Errorf("WebsocketURL %q does not match expected proxy shape", resp.WebsocketURL)
	}

	clientURL := resp.WebsocketURL

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	wsConn, _, err := websocket.Dial(dialCtx, clientURL, &websocket.DialOptions{
		HTTPClient: v.cpServer.Client(),
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer wsConn.Close(websocket.StatusInternalError, "")

	// Read the mock-agent's banner.
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	msgType, data, err := wsConn.Read(readCtx)
	if err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if msgType != websocket.MessageBinary {
		t.Errorf("banner message type = %v, want MessageBinary", msgType)
	}
	if !strings.Contains(string(data), "MOCK_AGENT_SERIAL_READY") {
		t.Errorf("banner = %q, want к contain MOCK_AGENT_SERIAL_READY", string(data))
	}

	// Send а frame; mock-agent uppercases-and-echoes.
	if err := wsConn.Write(readCtx, websocket.MessageBinary, []byte("hello")); err != nil {
		t.Fatalf("write echo input: %v", err)
	}
	echoMsgType, echo, err := wsConn.Read(readCtx)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if echoMsgType != websocket.MessageBinary {
		t.Errorf("echo message type = %v, want MessageBinary", echoMsgType)
	}
	if string(echo) != "HELLO" {
		t.Errorf("echo = %q, want HELLO", string(echo))
	}

	_ = wsConn.Close(websocket.StatusNormalClosure, "")
}

// TestVMConsole_VMNotRunning_409 locks the gate что the CP rejects
// console requests на VMs not в phase=running. The test stops the VM
// (transitions vm_runtime → stopped) and then expects 409.
func TestVMConsole_VMNotRunning_409(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-console-stopped", 0xc1, "private")

	vmName := "stopped-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Flip vm_runtime.phase к 'stopped'. The CP handler reads from
	// store directly, so writing the row is enough — no agent hop.
	if _, err := v.store.Pool().Exec(ctx, `UPDATE vm_runtime SET phase = 'stopped' WHERE vm_id = $1`, vmID); err != nil {
		t.Fatalf("update vm_runtime phase: %v", err)
	}

	status, body := v.postConsoleRequest(t, ctx, vmID, "serial", "")
	if status != http.StatusConflict {
		t.Fatalf("console POST status = %d, body = %s, want 409", status, body)
	}
	if !strings.Contains(string(body), `"code":"vm_not_running"`) {
		t.Errorf("body = %s, want code=vm_not_running", body)
	}
}

// TestVMConsole_ConsoleInUse_409 locks the per-VM single-session
// guard. Opens one WebSocket session, then attempts to open а second
// before closing the first; second attempt должно surface 409
// console_in_use. The CP proxy translates the agent's 409 verbatim.
func TestVMConsole_ConsoleInUse_409(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-console-lock", 0xc2, "private")

	vmName := "lock-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// First console: get token, dial WS, leave it open.
	status1, body1 := v.postConsoleRequest(t, ctx, vmID, "serial", "")
	if status1 != http.StatusOK {
		t.Fatalf("first console POST = %d, body = %s", status1, body1)
	}
	var resp1 vmConsoleResponse
	if err := json.Unmarshal(body1, &resp1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	dialURL1 := resp1.WebsocketURL

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	wsConn1, _, err := websocket.Dial(dialCtx, dialURL1, &websocket.DialOptions{
		HTTPClient: v.cpServer.Client(),
	})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer wsConn1.Close(websocket.StatusInternalError, "")
	// Drain the banner so the agent's read pump is actively engaged.
	if _, _, err := wsConn1.Read(dialCtx); err != nil {
		t.Fatalf("drain banner: %v", err)
	}

	// Second console: token issuance succeeds (the lock is а
	// connection-level guard, not а token guard), but the dial
	// itself returns 409 console_in_use.
	status2, body2 := v.postConsoleRequest(t, ctx, vmID, "serial", "")
	if status2 != http.StatusOK {
		t.Fatalf("second console POST = %d, body = %s", status2, body2)
	}
	var resp2 vmConsoleResponse
	if err := json.Unmarshal(body2, &resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	dialURL2 := resp2.WebsocketURL

	_, dialResp, err := websocket.Dial(dialCtx, dialURL2, &websocket.DialOptions{
		HTTPClient: v.cpServer.Client(),
	})
	if err == nil {
		t.Fatal("second dial succeeded, want failure due к console_in_use")
	}
	if dialResp == nil || dialResp.StatusCode != http.StatusConflict {
		t.Errorf("second dial response status = %v, want 409 (err=%v)", dialResp, err)
	}
}

// TestVMConsole_InvalidToken_401 locks the agent's token-validation
// path. Issues а token но dials с а fabricated value; CP proxy
// receives 401 от agent и echoes к client.
func TestVMConsole_InvalidToken_401(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-console-bad-token", 0xc3, "private")

	vmName := "bad-token-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Get а valid console URL, then poison the token query parameter.
	status, body := v.postConsoleRequest(t, ctx, vmID, "serial", "")
	if status != http.StatusOK {
		t.Fatalf("console POST = %d, body = %s", status, body)
	}
	var resp vmConsoleResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode console response: %v", err)
	}
	poisoned := strings.Replace(resp.WebsocketURL, resp.Token, "not-а-real-token", 1)

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	_, dialResp, err := websocket.Dial(dialCtx, poisoned, &websocket.DialOptions{
		HTTPClient: v.cpServer.Client(),
	})
	if err == nil {
		t.Fatal("dial с poisoned token succeeded, want failure")
	}
	if dialResp == nil || dialResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("dial response status = %v, want 401 (err=%v)", dialResp, err)
	}
}

// TestVMConsole_LongSession_NoTimeoutDrop locks the timeout-fix
// landing (closes the ~30s session-drop bug discovered post-Iteration
// Z + scheme detection): а WebSocket console session must survive
// past the configured ServerWriteTimeout/ReadTimeout (30s default)
// without dropping. Pre-fix the session terminated at ~30s because:
//  1. middleware.Timeout(30s) wrapped r.Context() — pump goroutines
//     inherited the deadline и returned ctx.Err() at 30s;
//  2. http.Server.WriteTimeout=30s set а deadline on the underlying
//     net.Conn via SetWriteDeadline at request start — the deadline
//     persisted on the hijacked WebSocket connection.
//
// The fix lifts console-stream out of the Timeout middleware Group
// и clears hijacked deadlines via http.NewResponseController. This
// test exercises both layers — а pump-context cancel WOULD reproduce
// regression #1; а TCP-deadline fire WOULD reproduce regression #2.
//
// 35-second wall-clock test (past the 30s boundary, short enough к
// keep integration-suite duration manageable). Five iterations of
// 7s sleep + write+read verification — six verification points around
// и past the boundary.
func TestVMConsole_LongSession_NoTimeoutDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-console-long", 0xc4, "private")

	vmName := "long-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	status, body := v.postConsoleRequest(t, ctx, vmID, "serial", "")
	if status != http.StatusOK {
		t.Fatalf("console POST status = %d, body = %s, want 200", status, body)
	}
	var resp vmConsoleResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode console response: %v", err)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	wsConn, _, err := websocket.Dial(dialCtx, resp.WebsocketURL, &websocket.DialOptions{
		HTTPClient: v.cpServer.Client(),
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer wsConn.Close(websocket.StatusInternalError, "")

	// Drain the mock-agent banner so the read pump is fully engaged
	// before the session timer starts.
	bannerCtx, bannerCancel := context.WithTimeout(ctx, 5*time.Second)
	defer bannerCancel()
	if _, _, berr := wsConn.Read(bannerCtx); berr != nil {
		t.Fatalf("read banner: %v", berr)
	}

	// Walk the session past the 30s middleware.Timeout boundary с а
	// fresh write+read cycle at each step. Pre-fix any iteration on or
	// after the boundary would have surfaced ctx.DeadlineExceeded на
	// the next Read OR а StatusGoingAway close frame от the agent's
	// pump unwinding.
	const stepCount = 5
	const stepInterval = 7 * time.Second
	sessionStart := time.Now()

	for i := 0; i < stepCount; i++ {
		time.Sleep(stepInterval)

		elapsed := time.Since(sessionStart)
		ioCtx, ioCancel := context.WithTimeout(ctx, 5*time.Second)
		probe := []byte{byte('a' + i)}
		if werr := wsConn.Write(ioCtx, websocket.MessageBinary, probe); werr != nil {
			ioCancel()
			t.Fatalf("write @ elapsed=%v failed (session dropped?): %v", elapsed, werr)
		}
		// Mock-agent uppercases-and-echoes; expect single byte back.
		msgType, echo, rerr := wsConn.Read(ioCtx)
		ioCancel()
		if rerr != nil {
			t.Fatalf("read @ elapsed=%v failed (session dropped?): %v", elapsed, rerr)
		}
		if msgType != websocket.MessageBinary {
			t.Errorf("step %d (elapsed=%v): message type = %v, want MessageBinary",
				i, elapsed, msgType)
		}
		if len(echo) != 1 || echo[0] != byte('A'+i) {
			t.Errorf("step %d (elapsed=%v): echo = %q, want %q",
				i, elapsed, echo, []byte{byte('A' + i)})
		}
	}

	totalElapsed := time.Since(sessionStart)
	if totalElapsed < 30*time.Second {
		t.Errorf("test concluded before the 30s timeout boundary it's meant to cross (elapsed=%v)",
			totalElapsed)
	}

	if err := wsConn.Close(websocket.StatusNormalClosure, "long-session test complete"); err != nil {
		t.Errorf("clean close failed after %v session: %v", totalElapsed, err)
	}
}
