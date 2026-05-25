// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// logsStreamBufSize bounds a single Read on the upstream agent body
// while relaying chunks downstream. Matches the agent-side pump
// buffer so we avoid splitting CSI sequences across writes in the
// common case.
const logsStreamBufSize = 4096

// Logs implements GET /v1/vms/{id}/logs - the CP-side proxy for the
// `otherix vm logs` operator workflow described in ADR 0029.
//
// Authorization runs in two layers:
//
//  1. middleware.RequirePermission(PermVMConsole) gates the route by
//     role capability (admin/operator/developer hold it; viewer does
//     not).
//  2. CheckOwnership enforces scope=own for developers; non-owners
//     receive 404 not_found per the project's no-existence-leak rule.
//
// The handler resolves the owning agent, opens its own HTTP stream
// against the agent's `/v1/vms/{name}/logs` endpoint (preserving the
// query string verbatim), and relays the chunked response to the
// client. Hijacked-connection deadlines are cleared before the first
// write so the long-lived stream is not killed at 30 s.
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	if h.consoleDeps.AgentClient == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "logs client not configured", nil)
		return
	}

	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vmName := chi.URLParam(r, "id")
	if vmName == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}

	vm, ok := h.loadLogsTargetVM(w, r, vmName)
	if !ok {
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMConsole); err != nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}

	agentURL, ok := h.buildLogsAgentURL(w, r, vm)
	if !ok {
		return
	}
	h.relayAgentLogs(w, r, agentURL, vmName)
}

// loadLogsTargetVM fetches the VM row by name and writes the error
// envelope for misses / DB errors. Returns (vm, true) on success.
func (h *Handler) loadLogsTargetVM(w http.ResponseWriter, r *http.Request, vmName string) (store.VM, bool) {
	vm, err := h.store.Queries().GetVMByName(r.Context(), vmName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeVMNotFound, "vm not found", nil)
			return store.VM{}, false
		}
		h.log.ErrorContext(r.Context(), "vms.logs load vm",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load vm", nil)
		return store.VM{}, false
	}
	return vm, true
}

// buildLogsAgentURL resolves vm -> node -> advertised endpoint and
// composes the agent-side logs URL. The CP forwards r.URL.RawQuery
// verbatim so `tail` and `follow` propagate without re-parsing.
func (h *Handler) buildLogsAgentURL(w http.ResponseWriter, r *http.Request, vm store.VM) (string, bool) {
	nodeID, err := h.resolveNodeForVM(r.Context(), vm)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs resolve node",
			"vm_id", vm.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve node", nil)
		return "", false
	}
	node, err := h.store.Queries().GetNodeByID(r.Context(), nodeID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs load node",
			"node_id", nodeID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return "", false
	}
	agentHost, err := stripScheme(node.AdvertisedEndpoint)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs agent endpoint shape",
			"endpoint", node.AdvertisedEndpoint, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "agent endpoint malformed", nil)
		return "", false
	}
	url := fmt.Sprintf("https://%s/v1/vms/%s/logs", agentHost, vm.Name)
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	return url, true
}

// relayAgentLogs opens the upstream stream and copies chunks to the
// client until either side closes. The HTTP client is rebuilt without
// a Timeout so the stream is not capped by the agentclient's per-poll
// budget; the underlying mTLS Transport is shared.
func (h *Handler) relayAgentLogs(w http.ResponseWriter, r *http.Request, agentURL, vmName string) {
	streamClient := &http.Client{Transport: h.consoleDeps.AgentClient.HTTPClient().Transport}
	// agentURL is composed from node.AdvertisedEndpoint + the URL-
	// param vmName. AdvertisedEndpoint is admin-controlled (set on
	// POST /v1/nodes); vmName is the chi URL param, not user-supplied
	// body. Treat as trusted - same pattern as console-stream proxy.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, agentURL, nil) //nolint:gosec // see comment above
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs build upstream request",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "build upstream request", nil)
		return
	}
	req.Header.Set("Accept", "text/plain")

	upstream, err := streamClient.Do(req) //nolint:gosec // see comment on NewRequestWithContext above
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs dial upstream",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusBadGateway,
			response.CodeAgentUnreachable, "logs relay dial failed", nil)
		return
	}
	defer func() { _ = upstream.Body.Close() }()

	if upstream.StatusCode != http.StatusOK {
		h.relayLogsUpstreamError(w, r, upstream)
		return
	}

	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.WarnContext(r.Context(), "vms.logs clear read deadline",
			"vm", vmName, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.WarnContext(r.Context(), "vms.logs clear write deadline",
			"vm", vmName, "error", derr.Error())
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flushIfPossible(w)

	buf := make([]byte, logsStreamBufSize)
	for {
		n, readErr := upstream.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flushIfPossible(w)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, r.Context().Err()) {
				h.log.WarnContext(r.Context(), "vms.logs upstream read",
					"vm", vmName, "error", readErr.Error())
			}
			return
		}
	}
}

// relayLogsUpstreamError translates non-2xx responses from the agent
// to the CP-facing envelope. We aim to surface the agent's status as
// faithfully as possible so the operator sees the actionable code.
func (h *Handler) relayLogsUpstreamError(w http.ResponseWriter, r *http.Request, upstream *http.Response) {
	switch upstream.StatusCode {
	case http.StatusNotFound:
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found on agent", nil)
	case http.StatusConflict:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm logs unavailable; restart the vm to re-enable streaming", nil)
	case http.StatusBadRequest:
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "agent rejected query parameters", nil)
	default:
		h.log.ErrorContext(r.Context(), "vms.logs upstream non-2xx",
			"status", upstream.StatusCode)
		response.WriteError(w, r, http.StatusBadGateway,
			response.CodeAgentUnreachable, "logs relay upstream error", nil)
	}
}

func flushIfPossible(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
