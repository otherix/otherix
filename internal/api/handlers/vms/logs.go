// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// logsStreamBufSize bounds a single Read on the upstream agent body while
// relaying chunks downstream.
const logsStreamBufSize = 4096

// migrationWaitDeadline bounds how long the follow loop waits for the cutover
// Txn to flip PinnedNodeID when the upstream breaks (or a re-dial returns 409)
// while a migration is still in flight. migrationPollInterval is the flip poll
// cadence. Both are a multiple of the typical sub-second cutover window;
// tolerant of slow convergence.
const (
	migrationWaitDeadline = 60 * time.Second
	migrationPollInterval = 500 * time.Millisecond
)

// logsNode is the resolved owning node for a follow stream: the node id
// (to detect a cutover flip) and the agent host (to dial).
type logsNode struct {
	id   uuid.UUID
	host string
}

// Logs implements GET /v1/vms/{id}/logs - the CP-side proxy for the
// `otherix vm logs` workflow. Authorization runs in two layers
// (RequirePermission(PermVMConsole) on the route; CheckOwnership here).
//
// For a follow stream the relay FOLLOWS the VM across a live migration:
// when the upstream breaks the CP re-resolves the current owner and
// re-dials it on the same client connection. Non-follow requests are a
// single-shot dump of the trailing history, unchanged.
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

	if r.URL.Query().Get("follow") == "true" {
		h.relayLogsFollowing(w, r, vm)
		return
	}

	// Single-shot (no follow): resolve once, pump once. The pump writes the
	// operator-facing envelope on a pre-stream failure, exactly as before.
	node, err := h.resolveLogsNode(r.Context(), vm)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs resolve node",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve node", nil)
		return
	}
	started := false
	h.pumpLogsOnce(w, r, h.logsStreamClient(),
		logsAgentURL(node.host, vmName, r.URL.RawQuery), vmName, &started)
}

// loadLogsTargetVM fetches the VM row by name and writes the error
// envelope for misses / DB errors. Returns (vm, true) on success.
func (h *Handler) loadLogsTargetVM(w http.ResponseWriter, r *http.Request, vmName string) (store.VM, bool) {
	vm, err := h.store.VMByName(r.Context(), vmName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
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

// logsStreamClient builds an HTTP client over the agentclient's mTLS
// Transport with no Timeout, so the long-lived stream is not capped by the
// per-poll budget.
func (h *Handler) logsStreamClient() *http.Client {
	return &http.Client{Transport: h.consoleDeps.AgentClient.HTTPClient().Transport}
}

// resolveLogsNode resolves vm -> owning node -> agent host. It prefers
// PinnedNodeID (set by the create handler and flipped by the cutover Txn),
// falling back to the storage pool's node.
func (h *Handler) resolveLogsNode(ctx context.Context, vm store.VM) (logsNode, error) {
	nodeID, err := h.resolveNodeForVM(ctx, vm)
	if err != nil {
		return logsNode{}, fmt.Errorf("resolve node: %w", err)
	}
	node, err := h.store.NodeByID(ctx, nodeID)
	if err != nil {
		return logsNode{}, fmt.Errorf("load node: %w", err)
	}
	host, err := stripScheme(node.AdvertisedEndpoint)
	if err != nil {
		return logsNode{}, fmt.Errorf("agent endpoint malformed: %w", err)
	}
	return logsNode{id: nodeID, host: host}, nil
}

// logsAgentURL composes the agent-side logs URL. rawQuery is forwarded
// verbatim (the client's tail/follow on the first attempt, "tail=-1&
// follow=true" on a re-dial). The host comes from node.AdvertisedEndpoint
// (admin-controlled) and the name from the chi URL param - trusted inputs,
// same as the console-stream proxy.
func logsAgentURL(agentHost, vmName, rawQuery string) string {
	u := fmt.Sprintf("https://%s/v1/vms/%s/logs", agentHost, vmName)
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

// logsPumpOutcome is the result of a single upstream attempt.
type logsPumpOutcome int

const (
	// logsPumpBroke: the upstream ended (clean EOF), returned a non-200 on a
	// re-dial, or the dial failed after the stream had already started. The
	// caller decides what to do from trustworthy store signals.
	logsPumpBroke logsPumpOutcome = iota
	// logsPumpClientGone: the downstream client disconnected or a write
	// failed.
	logsPumpClientGone
	// logsPumpPreStreamError: the FIRST dial failed or returned non-200
	// before any byte reached the client; pumpLogsOnce wrote the
	// operator-facing envelope.
	logsPumpPreStreamError
)

// pumpLogsOnce runs one upstream attempt against agentURL and relays its
// body to the client. *started tracks whether the downstream response has
// already begun (headers + first flush): a failure before *started writes
// the error envelope (logsPumpPreStreamError); a failure after *started
// cannot (headers are sent) and surfaces as logsPumpBroke for the caller's
// reconnect decision.
func (h *Handler) pumpLogsOnce(w http.ResponseWriter, r *http.Request, client *http.Client, agentURL, vmName string, started *bool) logsPumpOutcome {
	// agentURL is composed from admin-controlled node.AdvertisedEndpoint +
	// the URL-param vmName, not user body. Trusted - same as the console
	// proxy.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, agentURL, nil) //nolint:gosec // see comment above
	if err != nil {
		if !*started {
			h.log.ErrorContext(r.Context(), "vms.logs build upstream request",
				"vm", vmName, "error", err.Error())
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "build upstream request", nil)
			return logsPumpPreStreamError
		}
		return logsPumpBroke
	}
	req.Header.Set("Accept", "text/plain")

	upstream, err := client.Do(req) //nolint:gosec // see comment on NewRequestWithContext above
	if err != nil {
		if !*started {
			h.log.ErrorContext(r.Context(), "vms.logs dial upstream",
				"vm", vmName, "error", err.Error())
			response.WriteError(w, r, http.StatusBadGateway,
				response.CodeAgentUnreachable, "logs relay dial failed", nil)
			return logsPumpPreStreamError
		}
		return logsPumpBroke
	}
	defer func() { _ = upstream.Body.Close() }()

	if upstream.StatusCode != http.StatusOK {
		if !*started {
			h.relayLogsUpstreamError(w, r, upstream)
			return logsPumpPreStreamError
		}
		// A re-dial returned non-200 (e.g. 409 vm_not_running mid-handoff).
		// Headers are already sent; let the caller decide via store signals.
		return logsPumpBroke
	}

	if !*started {
		h.beginLogsStream(w, r, vmName)
		*started = true
	}

	buf := make([]byte, logsStreamBufSize)
	for {
		n, readErr := upstream.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return logsPumpClientGone
			}
			flushIfPossible(w)
		}
		if readErr != nil {
			if errors.Is(readErr, r.Context().Err()) {
				return logsPumpClientGone
			}
			if !errors.Is(readErr, io.EOF) {
				h.log.WarnContext(r.Context(), "vms.logs upstream read",
					"vm", vmName, "error", readErr.Error())
			}
			return logsPumpBroke
		}
	}
}

// relayLogsFollowing streams the VM's logs to the client, following the VM
// across live migrations. On an upstream break it re-reads the fresh
// PinnedNodeID: a node change (committed cutover) re-dials the target with
// tail=-1; same-node-no-migration is a clean end. A still-in-flight migration
// hits the safety-net branch that waits for the cutover flip.
func (h *Handler) relayLogsFollowing(w http.ResponseWriter, r *http.Request, vm store.VM) {
	client := h.logsStreamClient()
	vmName := vm.Name

	current, err := h.resolveLogsNode(r.Context(), vm)
	if err != nil {
		h.log.ErrorContext(r.Context(), "vms.logs resolve node",
			"vm", vmName, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve node", nil)
		return
	}

	started := false
	firstAttempt := true
	for {
		rawQuery := r.URL.RawQuery
		if !firstAttempt {
			rawQuery = "tail=-1&follow=true"
		}
		outcome := h.pumpLogsOnce(w, r, client,
			logsAgentURL(current.host, vmName, rawQuery), vmName, &started)
		firstAttempt = false

		if outcome == logsPumpClientGone || outcome == logsPumpPreStreamError {
			return
		}
		if r.Context().Err() != nil {
			return
		}

		fresh, err := h.store.VMByName(r.Context(), vmName)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				h.log.WarnContext(r.Context(), "vms.logs reattach load vm",
					"vm", vmName, "error", err.Error())
			}
			return
		}
		newNode, err := h.resolveLogsNode(r.Context(), fresh)
		if err != nil {
			h.log.WarnContext(r.Context(), "vms.logs reattach resolve node",
				"vm", vmName, "error", err.Error())
			return
		}
		if newNode.id != current.id {
			h.log.InfoContext(r.Context(), "vms.logs reattach",
				"vm", vmName, "from_node", current.id, "to_node", newNode.id)
			current = newNode
			continue
		}

		// Same node. A migration still in flight means the break (or a
		// mid-handoff 409) raced ahead of the cutover Txn flipping
		// PinnedNodeID - wait a bounded time for the flip rather than guess.
		_, migrating, err := h.store.ActiveMigrationForVM(r.Context(), vm.ID)
		if err != nil {
			h.log.WarnContext(r.Context(), "vms.logs reattach migration scan",
				"vm", vmName, "error", err.Error())
			return
		}
		if migrating {
			target, ok := h.waitForFlip(r.Context(), vmName, current.id,
				migrationWaitDeadline, migrationPollInterval)
			if !ok {
				return
			}
			h.log.InfoContext(r.Context(), "vms.logs reattach",
				"vm", vmName, "from_node", current.id, "to_node", target.id)
			current = target
			continue
		}

		// Same node, no migration -> clean end (today's behaviour).
		return
	}
}

// waitForFlip polls the VM's PinnedNodeID until it moves off fromNode (the
// cutover committed) or deadline elapses. It returns the new owning node on
// a flip, or ok=false on deadline / context cancellation / store error. An
// unresolvable endpoint mid-poll is transient (the target may not have
// advertised yet) and keeps polling until the deadline.
func (h *Handler) waitForFlip(ctx context.Context, vmName string, fromNode uuid.UUID, deadline, pollInterval time.Duration) (logsNode, bool) {
	timeout := time.NewTimer(deadline)
	defer timeout.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return logsNode{}, false
		case <-timeout.C:
			return logsNode{}, false
		case <-ticker.C:
			fresh, err := h.store.VMByName(ctx, vmName)
			if err != nil {
				return logsNode{}, false
			}
			n, err := h.resolveLogsNode(ctx, fresh)
			if err != nil {
				continue
			}
			if n.id != fromNode {
				return n, true
			}
		}
	}
}

// beginLogsStream clears the hijacked-connection deadlines and writes the
// streaming response headers on the first successful upstream. Deadline-clear
// failures are logged, not fatal - the response has already begun.
func (h *Handler) beginLogsStream(w http.ResponseWriter, r *http.Request, vmName string) {
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
}

// relayLogsUpstreamError translates non-2xx responses from the agent to the
// CP-facing envelope. Only reached before the stream starts.
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
