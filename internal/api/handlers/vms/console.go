// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// consoleClient is the narrow agentclient surface the CP console
// handler depends on. *agentclient.Client satisfies it structurally;
// tests substitute a stub through the Handler factory.
type consoleClient interface {
	IssueConsoleToken(ctx context.Context, endpoint, vmName, protocol string) (agentclient.IssueConsoleTokenResponse, error)
	HTTPClient() *http.Client
}

// ConsoleDeps is the dependency bundle the api binary supplies to the
// Handler so the console handler (POST /v1/vms/{id}/console) can
// dispatch to the pinned agent. AccessMode is the validated
// `config.ConsoleConfig.AccessMode` value — "proxy" (production
// default) or "direct" (lab/dev). Kept separate from LifecycleDeps so
// console-handler tests can mount the console primitive with a stub
// client without touching the sync lifecycle wiring.
type ConsoleDeps struct {
	AgentClient consoleClient
	AccessMode  string
}

// consoleResponse mirrors components/schemas/VMConsoleResponse in
// api/openapi/control-plane.yaml. Built handler-side instead of
// shared encoder so the wire format stays close to the OpenAPI
// contract for diff review.
type consoleResponse struct {
	Token        string `json:"token"`
	WebsocketURL string `json:"websocket_url"`
	Protocol     string `json:"protocol"`
	ExpiresAt    string `json:"expires_at"`
}

// consoleRequest is the optional body that selects the console
// protocol. Empty body / omitted field → defaults to "vnc" per the
// OpenAPI spec, but the agent today only implements serial. Operators
// must pass `{"protocol": "serial"}` to exercise the working path.
type consoleRequest struct {
	Protocol string `json:"protocol"`
}

// Console implements POST /v1/vms/{id}/console. Issues a single-use
// console session token by relaying to the pinned agent, builds the
// websocket URL according to the configured access mode (proxy /
// direct), and returns the operator-facing envelope.
//
// Required permission: `vm:console` (admin / operator: any;
// developer: own; viewer: none). Cross-user developer attempts
// surface as 404 (no leak) per the project's "403 vs 404" rule.
func (h *Handler) Console(w http.ResponseWriter, r *http.Request) {
	if h.consoleDeps.AgentClient == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "console client not configured", nil)
		return
	}

	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vm, err := resolver.VM(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writeResolveError(w, r, err)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMConsole); err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}

	protocol, ok := parseConsoleProtocol(w, r)
	if !ok {
		return
	}

	// VM must be running before the agent can usefully proxy console
	// bytes. Phase check uses vm_runtime since that's the agent-
	// reported observed phase; desired_phase=running with no runtime
	// row means the VM is still booting and returns 409 vm_not_running.
	runtime, err := h.store.Queries().GetVMRuntime(r.Context(), vm.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeVMNotRunning, "vm has not reported runtime state yet", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "vms.console load runtime",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load runtime", nil)
		return
	}
	if runtime.Phase != store.VmPhaseRunning {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm is not running",
			map[string]any{"current_phase": string(runtime.Phase)})
		return
	}

	nodeID, err := h.resolveNodeForVM(r.Context(), vm)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.console resolve node",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve node", nil)
		return
	}
	node, err := h.store.Queries().GetNodeByID(r.Context(), nodeID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.console load node",
			"node_id", nodeID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return
	}

	agentResp, err := h.consoleDeps.AgentClient.IssueConsoleToken(
		r.Context(), node.AdvertisedEndpoint, vm.Name, protocol)
	if err != nil {
		writeConsoleAgentError(w, r, h.log, err)
		return
	}

	wsURL, err := buildConsoleWebsocketURL(h.consoleDeps.AccessMode, r, node.AdvertisedEndpoint, vm.Name, agentResp)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.console build url",
			"vm_id", vm.ID, "mode", h.consoleDeps.AccessMode, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "build websocket url", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, consoleResponse{
		Token:        agentResp.Token,
		WebsocketURL: wsURL,
		Protocol:     agentResp.Protocol,
		ExpiresAt:    agentResp.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	})
}

// parseConsoleProtocol decodes the optional body. Returns ("vnc", true)
// for empty body, the supplied value otherwise. Unsupported protocols
// return (..., false) after writing 400 validation_failed to the
// response writer; caller should bail.
func parseConsoleProtocol(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return "vnc", true
	}
	var body consoleRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body: "+err.Error(), nil)
		return "", false
	}
	if body.Protocol == "" {
		return "vnc", true
	}
	switch body.Protocol {
	case "vnc", "spice", "serial":
		return body.Protocol, true
	default:
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"unsupported protocol",
			map[string]any{"protocol": body.Protocol, "supported": []string{"vnc", "spice", "serial"}})
		return "", false
	}
}

// writeConsoleAgentError translates agentclient errors to the
// operator-facing envelope. agentclient surfaces non-2xx as
// *agentclient.AgentError carrying the agent's own code; the CP
// echoes the most relevant codes (vm_not_running, console_in_use,
// protocol_not_available) verbatim so the operator-facing diagnosis
// is preserved.
func writeConsoleAgentError(w http.ResponseWriter, r *http.Request, log interface {
	ErrorContext(ctx context.Context, msg string, args ...any)
}, err error,
) {
	var agentErr *agentclient.AgentError
	if errors.As(err, &agentErr) {
		switch agentErr.Status {
		case http.StatusNotFound:
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeVMNotFound, "vm not found on owning agent", nil)
			return
		case http.StatusConflict:
			code := response.ErrorCode(agentErr.Code)
			switch code {
			case response.CodeVMNotRunning, response.CodeConsoleInUse, response.CodeProtocolNotAvailable:
				response.WriteError(w, r, http.StatusConflict, code,
					agentErr.Message, agentErr.Details)
				return
			}
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, agentErr.Message, agentErr.Details)
			return
		case http.StatusBadRequest:
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, agentErr.Message, agentErr.Details)
			return
		}
	}
	log.ErrorContext(r.Context(), "vms.console agent dispatch",
		"error", err.Error())
	response.WriteError(w, r, http.StatusBadGateway,
		response.CodeAgentUnreachable, "console token issuance failed", nil)
}

// buildConsoleWebsocketURL composes the ws(s):// URL the CLI dials. In
// proxy mode the CP returns its own host so the operator's CLI never
// reaches an agent directly; in direct mode the URL points to the
// agent's advertised endpoint, bypassing the CP data path.
//
// Proxy-mode scheme tracks the incoming request: ws:// when the CP
// listener serves plain HTTP (local dev), wss:// when TLS is terminated
// either at the listener itself or upstream at a reverse proxy that
// forwards `X-Forwarded-Proto`. Direct mode always emits wss:// — the
// agent serves HTTPS via mTLS unconditionally.
func buildConsoleWebsocketURL(mode string, r *http.Request, agentEndpoint, vmName string, agentResp agentclient.IssueConsoleTokenResponse) (string, error) {
	switch mode {
	case "", "proxy":
		// CP-facing relay. Mirror the operator's incoming host —
		// that's the URL they used to reach us, and by definition reachable.
		host := r.Host
		if host == "" {
			return "", errors.New("proxy mode requires a Host header on the incoming request")
		}
		wsScheme := httpSchemeToWebSocket(detectScheme(r))
		return fmt.Sprintf("%s://%s/v1/vms/%s/console-stream?token=%s",
			wsScheme, host, vmName, agentResp.Token), nil
	case "direct":
		// agentEndpoint is `https://host:port`; swap to wss:// and tail
		// in the agent-supplied websocket path. The agent owns the
		// path shape; we only host-swap. Agent always serves HTTPS
		// via mTLS — no scheme detection here.
		hostPart, err := stripScheme(agentEndpoint)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("wss://%s%s?token=%s",
			hostPart, agentResp.WebsocketPath, agentResp.Token), nil
	default:
		return "", fmt.Errorf("unsupported console access mode %q", mode)
	}
}

// detectScheme returns "http" or "https" for the operator-facing leg
// of a CP request. Priority:
//
//  1. `X-Forwarded-Proto` header (reverse proxy with TLS termination —
//     the proxy speaks HTTPS to the operator and plain HTTP to CP).
//     Comma-separated chains (multi-hop) are resolved by taking the
//     leftmost entry (closest to the operator). Values other than
//     "http" or "https" are ignored.
//  2. `r.TLS != nil` — direct TLS termination at the CP listener.
//  3. Fallback "http" — plain-HTTP dev and test setups.
//
// This is the WebSocket-URL composition helper only; do not use it to
// gate auth or any security-sensitive decision (operator-controlled
// header).
func detectScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if idx := strings.IndexByte(proto, ','); idx >= 0 {
			proto = proto[:idx]
		}
		proto = strings.TrimSpace(strings.ToLower(proto))
		switch proto {
		case "http", "https":
			return proto
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// httpSchemeToWebSocket maps the operator-facing HTTP scheme to its
// WebSocket counterpart (http→ws, https→wss). Unknown inputs fall
// back to "ws" — defensive default since detectScheme is the only
// caller and returns the canonical pair.
func httpSchemeToWebSocket(httpScheme string) string {
	if httpScheme == "https" {
		return "wss"
	}
	return "ws"
}

// stripScheme returns the host:port piece of a scheme-prefixed URL.
// Accepts `https://host:port` and `http://host:port`; rejects bare
// hosts (operator misconfiguration we want to surface loudly rather
// than silently producing an invalid wss:// URL).
func stripScheme(endpoint string) (string, error) {
	const httpsPrefix = "https://"
	const httpPrefix = "http://"
	if len(endpoint) > len(httpsPrefix) && endpoint[:len(httpsPrefix)] == httpsPrefix {
		return endpoint[len(httpsPrefix):], nil
	}
	if len(endpoint) > len(httpPrefix) && endpoint[:len(httpPrefix)] == httpPrefix {
		return endpoint[len(httpPrefix):], nil
	}
	return "", fmt.Errorf("agent endpoint %q must start with https:// or http://", endpoint)
}
