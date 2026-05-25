// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// consoleStreamBufSize bounds а single read from the QEMU `-serial`
// Unix socket. Serial output rate is keyboard-fast (login prompt,
// boot messages) so the buffer earns its keep by amortising the
// system call cost rather than chasing throughput.
const consoleStreamBufSize = 4096

// consoleTokenRequest mirrors the agent.yaml ConsoleTokenRequest
// schema. We do not import the generated type to keep the package
// chi-router-shaped (the generated ServerInterface is wired only по
// the mock-agent surface today).
type consoleTokenRequest struct {
	Protocol string `json:"protocol"`
}

// consoleTokenResponse mirrors agent.yaml ConsoleTokenResponse.
type consoleTokenResponse struct {
	Token         string    `json:"token"`
	ExpiresAt     time.Time `json:"expires_at"`
	WebsocketPath string    `json:"websocket_path"`
	Protocol      string    `json:"protocol"`
}

// ConsoleIssueToken handles POST /v1/vms/{vm_name}/console-token.
// Validates the VM exists и is running, mints а single-use token
// scoped к (vm, protocol) via the agent's TokenStore, returns the
// token + websocket path to the caller (the CP). The agent stores
// only sha256(plaintext); the plaintext returned here is the only
// copy.
func (h *Handler) ConsoleIssueToken(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}

	v, err := h.manager.ByName(name)
	if err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "vm not found", nil)
			return
		}
		h.log.Error("console: resolve vm failed", "vm", name, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}

	if v.Status != vm.StatusRunning {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm is not running",
			map[string]any{"current_status": string(v.Status)})
		return
	}

	protocol := console.ProtocolVNC
	if r.Body != nil && r.ContentLength != 0 {
		var body consoleTokenRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid request body: "+err.Error(), nil)
			return
		}
		if body.Protocol != "" {
			protocol = console.Protocol(body.Protocol)
		}
	}
	if !protocol.Valid() {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"unsupported console protocol",
			map[string]any{"protocol": string(protocol)})
		return
	}

	// `serial` requires the VM к expose а Unix-socket chardev; agents
	// что lack ConsoleSocket return 409 protocol_not_available rather
	// than 500 — the CP surfaces this к the operator с the same code.
	if protocol == console.ProtocolSerial && v.ConsoleSocket == "" {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeProtocolNotAvailable,
			"vm does not expose а serial chardev (no -serial unix: socket)",
			map[string]any{"protocol": "serial"})
		return
	}

	raw, expiresAt, err := h.tokens.Issue(name, protocol)
	if err != nil {
		h.log.Error("console: token issuance failed", "vm", name, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, consoleTokenResponse{
		Token:         raw,
		ExpiresAt:     expiresAt,
		WebsocketPath: "/v1/vms/" + name + "/console-stream",
		Protocol:      string(protocol),
	})
}

// ConsoleStream handles GET /v1/vms/{vm_name}/console-stream. Validates
// the token (single-use, TTL, VM binding), acquires the per-VM
// connection lock (returns 409 console_in_use на concurrent connect),
// upgrades к WebSocket, и pumps bytes bidirectionally between the
// WebSocket and the QEMU `-serial unix:` socket. Pump goroutines
// cancel the shared context on either side closing so the other side
// unwinds promptly.
func (h *Handler) ConsoleStream(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return
	}

	rawToken := r.URL.Query().Get("token")
	if rawToken == "" {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing token", nil)
		return
	}

	token, err := h.tokens.Consume(rawToken, name)
	if err != nil {
		// All token-validation paths surface as 401 — the operator-facing
		// distinction between "expired" vs "wrong vm" leaks information
		// против а probing client.
		h.log.Debug("console: token rejected",
			"vm", name, "reason", err.Error())
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "invalid or expired token", nil)
		return
	}

	if token.Protocol != console.ProtocolSerial {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeProtocolNotAvailable,
			"only serial protocol is implemented on this agent today",
			map[string]any{"requested": string(token.Protocol)})
		return
	}

	v, err := h.manager.ByName(name)
	if err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "vm not found", nil)
			return
		}
		h.log.Error("console: resolve vm at stream time failed",
			"vm", name, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}
	if v.Status != vm.StatusRunning {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm is not running",
			map[string]any{"current_status": string(v.Status)})
		return
	}
	if v.ConsoleSocket == "" {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeProtocolNotAvailable,
			"vm does not expose а serial chardev (no -serial unix: socket)", nil)
		return
	}

	if err := h.conns.Acquire(name); err != nil {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConsoleInUse,
			"another console session is already open for this vm", nil)
		return
	}
	defer h.conns.Release(name)

	unixConn, err := net.Dial("unix", v.ConsoleSocket) //nolint:gosec // ConsoleSocket path is computed agent-side from m.stateDir + vmID — not operator input
	if err != nil {
		h.log.Error("console: dial serial socket",
			"vm", name, "socket", v.ConsoleSocket, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "console socket unavailable", nil)
		return
	}
	defer func() { _ = unixConn.Close() }()

	// Clear hijacked connection deadlines — http.Server.ReadTimeout /
	// WriteTimeout are applied via SetRead/WriteDeadline at request
	// start, и persist on the net.Conn after coder/websocket.Accept
	// hijacks it. Without these calls, the WebSocket session drops at
	// ~30s when those deadlines fire. Logged at WARN instead of bailing
	// — а ResponseController error shouldn't kill an otherwise-healthy
	// upgrade path.
	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.Warn("console: clear read deadline", "vm", name, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.Warn("console: clear write deadline", "vm", name, "error", derr.Error())
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// CP-side proxy mode dials over mTLS от а non-browser process,
		// so origin check is irrelevant; CLI direct mode also bypasses
		// the browser. The token + mTLS-from-caller is the auth contract.
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Error("console: websocket upgrade failed",
			"vm", name, "error", err.Error())
		return
	}
	defer func() { _ = wsConn.Close(websocket.StatusInternalError, "") }()

	h.pumpConsole(r.Context(), name, wsConn, unixConn)
}

// pumpConsole runs the bidirectional copy between WebSocket frames
// and the Unix socket carrying QEMU's serial byte stream. Both pumps
// share а context; the first side to close cancels it и the other
// pump unwinds. WebSocket frames are binary (no framing wrapper, no
// resize protocol).
func (h *Handler) pumpConsole(parent context.Context, vmName string, wsConn *websocket.Conn, unixConn net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer cancel()
		// Force unixConn.Read к unblock once the context goes down so
		// the unix→ws pump doesn't sit forever after ws→unix closed.
		go func() {
			<-ctx.Done()
			_ = unixConn.SetReadDeadline(time.Now())
		}()

		buf := make([]byte, consoleStreamBufSize)
		for {
			n, err := unixConn.Read(buf)
			if n > 0 {
				if werr := wsConn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer cancel()
		for {
			_, data, err := wsConn.Read(ctx)
			if err != nil {
				return
			}
			if _, werr := unixConn.Write(data); werr != nil {
				return
			}
		}
	}()

	wg.Wait()
	_ = wsConn.Close(websocket.StatusNormalClosure, "")
	h.log.Info("console: session closed", "vm", vmName)
}
