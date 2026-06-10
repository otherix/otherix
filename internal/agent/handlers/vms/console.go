// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/serialmux"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/wskeepalive"
)

// consoleTokenRequest mirrors the agent.yaml ConsoleTokenRequest
// schema. We do not import the generated type to keep the package
// chi-router-shaped (the generated ServerInterface is wired only by
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
// Validates the VM exists and is running, mints a single-use token
// scoped to (vm, protocol) via the agent's TokenStore, returns the
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

	// `serial` requires the VM to expose a Unix-socket chardev; agents
	// that lack ConsoleSocket return 409 protocol_not_available rather
	// than 500 — the CP surfaces this to the operator with the same code.
	if protocol == console.ProtocolSerial && v.ConsoleSocket == "" {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeProtocolNotAvailable,
			"vm does not expose a serial chardev (no -serial unix: socket)",
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
// the token (single-use, TTL, VM binding), attaches a console
// subscriber to the per-VM multiplexer (the multiplexer enforces the
// single-active-console invariant via serialmux.ErrConsoleInUse, the
// 409 maps to console_in_use), upgrades to WebSocket, and pumps
// bytes bidirectionally between the WebSocket and the multiplexer's
// subscriber channel. The subscriber receives a 20-line history tail
// plus a visual separator before live bytes, per ADR 0029 L18.
func (h *Handler) ConsoleStream(w http.ResponseWriter, r *http.Request) {
	name, token, ok := h.consoleStreamPrelude(w, r)
	if !ok {
		return
	}
	v, ok := h.consoleStreamLoadVM(w, r, name)
	if !ok {
		return
	}
	if !h.consoleStreamCheckToken(w, r, name, token) {
		return
	}
	if !h.consoleStreamCheckRunnable(w, r, v) {
		return
	}

	mux := h.manager.GetMux(name)
	if mux == nil {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm has no active multiplexer; restart the vm to re-enable console", nil)
		return
	}
	sub, err := mux.SubscribeConsole()
	if err != nil {
		if errors.Is(err, serialmux.ErrConsoleInUse) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConsoleInUse,
				"another console session is already open for this vm", nil)
			return
		}
		h.log.Error("console: subscribe failed", "vm", name, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "console subscribe failed", nil)
		return
	}
	defer func() { _ = sub.Close() }()

	// Clear hijacked connection deadlines so the long-lived WebSocket
	// is not killed at ReadTimeout / WriteTimeout once the response
	// has been hijacked. Logged at WARN but not fatal.
	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.Warn("console: clear read deadline", "vm", name, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.Warn("console: clear write deadline", "vm", name, "error", derr.Error())
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Error("console: websocket upgrade failed",
			"vm", name, "error", err.Error())
		return
	}
	defer func() { _ = wsConn.Close(websocket.StatusInternalError, "") }()

	h.pumpConsoleViaMux(r.Context(), name, wsConn, sub)
}

// consoleStreamPrelude pulls vm_name + token out of the request and
// writes the relevant error envelope if either is missing.
func (h *Handler) consoleStreamPrelude(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "vm not found", nil)
		return "", "", false
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing token", nil)
		return "", "", false
	}
	return name, token, true
}

// consoleStreamCheckToken consumes the token via the TokenStore. The
// operator-facing distinction between "expired" / "wrong vm" is not
// surfaced - we collapse every token failure to 401 so a probing
// client cannot disambiguate.
func (h *Handler) consoleStreamCheckToken(w http.ResponseWriter, r *http.Request, name, raw string) bool {
	token, err := h.tokens.Consume(raw, name)
	if err != nil {
		h.log.Debug("console: token rejected",
			"vm", name, "reason", err.Error())
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "invalid or expired token", nil)
		return false
	}
	if token.Protocol != console.ProtocolSerial {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeProtocolNotAvailable,
			"only serial protocol is implemented on this agent today",
			map[string]any{"requested": string(token.Protocol)})
		return false
	}
	return true
}

// consoleStreamLoadVM resolves the VM at stream time (the operator
// could have stopped or deleted it between token issuance and the
// WebSocket dial). Errors / misses surface to the caller before the
// upgrade happens so they receive a JSON envelope rather than an
// abruptly-closed WebSocket.
func (h *Handler) consoleStreamLoadVM(w http.ResponseWriter, r *http.Request, name string) (*vm.VM, bool) {
	v, err := h.manager.ByName(name)
	if err != nil {
		if errors.Is(err, vm.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "vm not found", nil)
			return nil, false
		}
		h.log.Error("console: resolve vm at stream time failed",
			"vm", name, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return nil, false
	}
	return v, true
}

// consoleStreamCheckRunnable rejects requests when the VM is not in
// a phase that admits a serial console attachment.
func (h *Handler) consoleStreamCheckRunnable(w http.ResponseWriter, r *http.Request, v *vm.VM) bool {
	if v.Status != vm.StatusRunning {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm is not running",
			map[string]any{"current_status": string(v.Status)})
		return false
	}
	if v.ConsoleSocket == "" {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeProtocolNotAvailable,
			"vm does not expose a serial chardev (no -serial unix: socket)", nil)
		return false
	}
	return true
}

// pumpConsoleViaMux runs the bidirectional copy between the
// WebSocket and the multiplexer's console subscriber. Both pumps
// share a context; the first side to close cancels it and the other
// pump unwinds. WebSocket frames are binary (no framing wrapper, no
// resize protocol).
func (h *Handler) pumpConsoleViaMux(parent context.Context, vmName string, wsConn *websocket.Conn, sub *serialmux.ConsoleSubscriber) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)

	// keepalive: ping the operator (the CP in proxy mode, the operator
	// directly in direct mode) so a half-open / dead session is detected
	// and cancelled - which unwinds the pumps below and runs the deferred
	// sub.Close(), freeing the single-console slot. Without it an abrupt
	// disconnect leaves the slot held until the agent restarts.
	go func() {
		defer wg.Done()
		wskeepalive.Run(ctx, cancel, wsConn, wskeepalive.DefaultInterval, wskeepalive.DefaultTimeout)
	}()

	// mux -> websocket pump
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			select {
			case data, ok := <-sub.Bytes():
				if !ok {
					return
				}
				if werr := wsConn.Write(ctx, websocket.MessageBinary, data); werr != nil {
					return
				}
			case <-sub.Done():
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// websocket -> mux pump (operator input)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			_, data, err := wsConn.Read(ctx)
			if err != nil {
				return
			}
			if werr := sub.Write(data); werr != nil {
				return
			}
		}
	}()

	wg.Wait()
	_ = wsConn.Close(websocket.StatusNormalClosure, "")
	h.log.Info("console: session closed", "vm", vmName)
}
