// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/response"
)

// ConsoleStream implements GET /v1/vms/{id}/console-stream — the
// CP-side proxy WebSocket relay for proxy mode.
// The operator's CLI hits this URL with the agent-issued token in the
// query string; the CP resolves the owning agent, opens its own
// WebSocket to the agent's `console-stream` endpoint forwarding the
// token verbatim, and pumps binary frames bidirectionally. The token
// is the auth contract — the agent validates it, the CP only
// transports.
//
// Direct mode operators bypass this handler entirely: the CP's
// `vms.console` response carries the agent's external URL directly,
// so the CLI dials the agent without crossing this handler.
//
// This handler is intentionally anonymous (no Authn middleware): the
// agent token in the query string is the auth credential, and
// requiring a user JWT here would force the CLI to maintain a second
// credential path in parallel.
func (h *Handler) ConsoleStream(w http.ResponseWriter, r *http.Request) {
	if h.consoleDeps.AgentClient == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "console client not configured", nil)
		return
	}

	vmName := chi.URLParam(r, "id")
	if vmName == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing token", nil)
		return
	}

	vm, err := h.store.Queries().GetVMByName(r.Context(), vmName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeVMNotFound, "vm not found", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "vms.consoleStream load vm",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load vm", nil)
		return
	}
	nodeID, err := h.resolveNodeForVM(r.Context(), vm)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.consoleStream resolve node",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve node", nil)
		return
	}
	node, err := h.store.Queries().GetNodeByID(r.Context(), nodeID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.consoleStream load node",
			"node_id", nodeID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return
	}

	agentHost, err := stripScheme(node.AdvertisedEndpoint)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.consoleStream agent endpoint shape",
			"endpoint", node.AdvertisedEndpoint, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "agent endpoint malformed", nil)
		return
	}
	agentURL := fmt.Sprintf("wss://%s/v1/vms/%s/console-stream?token=%s",
		agentHost, vmName, token)

	// Dial the agent's WebSocket using the agentclient's mTLS-configured
	// HTTP client. Agent validates the token before completing the
	// handshake; non-101 surfaces as a Dial error here, and we relay
	// the agent's status / body to the operator's client as 5xx
	// (we cannot return 4xx after the upgrade has begun, and agent-
	// reported 401 / 409 should reach the operator with their full
	// fidelity — coder/websocket carries the original response).
	upstream, upstreamResp, err := websocket.Dial(r.Context(), agentURL, &websocket.DialOptions{
		HTTPClient: h.consoleDeps.AgentClient.HTTPClient(),
	})
	if err != nil {
		h.relayUpstreamDialError(w, r, upstreamResp, err)
		return
	}
	defer func() { _ = upstream.Close(websocket.StatusInternalError, "") }()

	// Clear hijacked connection deadlines — http.Server.ReadTimeout /
	// WriteTimeout are applied via SetRead/WriteDeadline at request
	// start, and persist on the net.Conn after coder/websocket.Accept
	// hijacks it. Without these calls, the WebSocket relay drops at
	// ~30s when those deadlines fire. Logged at WARN instead of bailing.
	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.WarnContext(r.Context(), "vms.consoleStream clear read deadline",
			"vm", vmName, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.WarnContext(r.Context(), "vms.consoleStream clear write deadline",
			"vm", vmName, "error", derr.Error())
	}

	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept already wrote a response; nothing to do beyond cleaning
		// up the upstream connection (deferred above).
		h.log.ErrorContext(r.Context(), "vms.consoleStream downstream accept",
			"vm", vmName, "error", err.Error())
		return
	}
	defer func() { _ = downstream.Close(websocket.StatusInternalError, "") }()

	h.relayConsoleFrames(r.Context(), vmName, downstream, upstream)
}

// relayUpstreamDialError translates failures dialing the agent's
// WebSocket to the operator-facing envelope. WebSocket.Dial preserves
// the upstream HTTP response when available, so the agent's 401 /
// 409 codes can be echoed verbatim before we burn through to 502.
func (h *Handler) relayUpstreamDialError(w http.ResponseWriter, r *http.Request, upstreamResp *http.Response, err error) {
	if upstreamResp != nil {
		switch upstreamResp.StatusCode {
		case http.StatusUnauthorized:
			response.WriteError(w, r, http.StatusUnauthorized,
				response.CodeUnauthenticated, "agent rejected the token", nil)
			return
		case http.StatusConflict:
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConsoleInUse,
				"console session already open or vm not running on the agent", nil)
			return
		case http.StatusNotFound:
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeVMNotFound, "vm not found on agent", nil)
			return
		}
	}
	h.log.ErrorContext(r.Context(), "vms.consoleStream dial upstream",
		"error", err.Error())
	response.WriteError(w, r, http.StatusBadGateway,
		response.CodeAgentUnreachable, "console relay dial failed", nil)
}

// relayConsoleFrames pumps binary WebSocket frames in both directions
// between downstream (operator) and upstream (agent). First close on
// either side cancels the shared context and the other pump unwinds.
// Frame types pass through verbatim.
func (h *Handler) relayConsoleFrames(parent context.Context, vmName string, downstream, upstream *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := downstream.Read(ctx)
			if err != nil {
				return
			}
			if werr := upstream.Write(ctx, typ, data); werr != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := upstream.Read(ctx)
			if err != nil {
				return
			}
			if werr := downstream.Write(ctx, typ, data); werr != nil {
				return
			}
		}
	}()

	wg.Wait()
	_ = upstream.Close(websocket.StatusNormalClosure, "")
	_ = downstream.Close(websocket.StatusNormalClosure, "")
	h.log.Info("vms.consoleStream relay closed", "vm", vmName)
}
