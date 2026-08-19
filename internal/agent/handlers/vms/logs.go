// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/api/response"
)

// Logs handles GET /v1/vms/{vm_name}/logs. The response
// is a chunked text/plain stream of sanitized serial console output.
//
// Query parameters:
//   - tail (default -1): trailing N lines from history; -1 yields
//     every available byte, 0 skips history entirely.
//   - follow (default false): continue streaming live bytes after
//     the initial history is delivered. Without follow, the response
//     ends after the initial tail is flushed.
//
// The handler attaches a serialmux.LogsSubscriber to the per-VM
// multiplexer (Manager.GetMux). When no multiplexer is registered
// (the VM is gone or the agent restarted while the VM was running)
// it returns 409 vm_not_running - operator restarts the VM to
// re-enable streaming.
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "vm_name")
	if name == "" {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return
	}
	v, ok := h.resolveLogsTarget(w, r, name)
	if !ok {
		return
	}
	mux := h.manager.GetMux(v.ID)
	if mux == nil {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeVMNotRunning,
			"vm logs unavailable; restart the vm to re-enable streaming", nil)
		return
	}
	tail, follow, perr := parseLogParams(r)
	if perr != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, perr.Error(), nil)
		return
	}
	sub := mux.SubscribeLogs(tail, follow)
	defer func() { _ = sub.Close() }()

	h.prepareLogsStream(w, name)
	h.streamLogs(w, r, sub)
}

// resolveLogsTarget resolves the named VM, whose id keys the
// multiplexer registry. Returns the VM and true on success; on miss /
// internal error it writes the response envelope and the caller bails
// out.
func (h *Handler) resolveLogsTarget(w http.ResponseWriter, r *http.Request, name string) (*vm.VM, bool) {
	v, err := h.manager.ByName(name)
	if err == nil {
		return v, true
	}
	if errors.Is(err, vm.ErrNotFound) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeVMNotFound, "vm not found", nil)
		return nil, false
	}
	h.log.Error("logs: resolve vm failed", "vm", name, "error", err.Error())
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "internal error", nil)
	return nil, false
}

// prepareLogsStream clears the hijacked-connection deadlines and
// writes the streaming response headers. Deadline-clear failures are
// logged but not fatal - the upgrade has already happened.
func (h *Handler) prepareLogsStream(w http.ResponseWriter, name string) {
	rc := http.NewResponseController(w)
	if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
		h.log.Warn("logs: clear read deadline", "vm", name, "error", derr.Error())
	}
	if derr := rc.SetWriteDeadline(time.Time{}); derr != nil {
		h.log.Warn("logs: clear write deadline", "vm", name, "error", derr.Error())
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flushResponse(w)
}

// streamLogs copies subscriber bytes to w until the subscriber Done
// fires, the client disconnects, or a write fails. On Done the
// remaining buffered bytes are drained best-effort before returning.
func (h *Handler) streamLogs(w http.ResponseWriter, r *http.Request, sub logsSink) {
	for {
		select {
		case chunk, ok := <-sub.Bytes():
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flushResponse(w)
		case <-sub.Done():
			drainSubscriber(w, sub)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// drainSubscriber writes any pending chunks queued on the subscriber
// channel after Done fired. Errors from w.Write are swallowed because
// the client has already gone or will see EOF.
func drainSubscriber(w http.ResponseWriter, sub logsSink) {
	for {
		select {
		case chunk := <-sub.Bytes():
			_, _ = w.Write(chunk)
		default:
			flushResponse(w)
			return
		}
	}
}

// logsSink is the minimum surface streamLogs / drainSubscriber need
// from a LogsSubscriber. Keeping it tight makes streamLogs trivially
// testable with a fake.
type logsSink interface {
	Bytes() <-chan []byte
	Done() <-chan struct{}
}

// parseLogParams pulls `tail` and `follow` from the query string with
// the defaults from the spec (tail=-1, follow=false). An unparsable
// `tail` value is surfaced to the caller as a validation failure
// instead of silently falling back to the default.
func parseLogParams(r *http.Request) (tail int, follow bool, err error) {
	tail = -1
	if v := r.URL.Query().Get("tail"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil {
			return 0, false, errors.New("tail must be an integer")
		}
		tail = n
	}
	follow = r.URL.Query().Get("follow") == "true"
	return tail, follow, nil
}

func flushResponse(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
