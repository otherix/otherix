// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// APIError is the typed error every Client method returns when the CP
// responds with a non-2xx envelope. Mirrors the shape of
// internal/api/response.ErrorBody — separate type so the CLI does not
// import the server-side response package.
//
// Callers can inspect Code (the stable string from the spec's ErrorCode
// catalog) to decide remediation: e.g. `vm_not_found` is operator
// input error; `agent_unreachable` is transient and should retry.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
}

// Error renders the canonical operator-facing string. Format mirrors
// ping-agent's classifier ("api_error: <code>: <message>") so shell
// scripts that grep on the prefix keep working across CLI surfaces.
func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("api_error: status %d", e.StatusCode)
	}
	return fmt.Sprintf("api_error: %s: %s", e.Code, e.Message)
}

// IsRetryable reports whether the underlying status warrants a
// transparent retry — 408, 429, 5xx. 4xx (except those two) are
// permanent input or auth errors. Callers (most interestingly
// WaitTask) use this to decide between continuing the polling loop
// and aborting.
func (e *APIError) IsRetryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return e.StatusCode >= 500
}

// envelope is the inner wire shape we decode from. The package keeps
// this private — callers only see the typed APIError.
type envelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	} `json:"error"`
}

// parseAPIError decodes the response body into an APIError. A missing
// or malformed envelope still yields a non-nil error so callers do
// not have to null-check separately from the status code; the Code stays
// empty in that case.
func parseAPIError(status int, body []byte) *APIError {
	out := &APIError{StatusCode: status}
	if len(body) == 0 {
		return out
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return out
	}
	out.Code = env.Error.Code
	out.Message = env.Error.Message
	out.Details = env.Error.Details
	return out
}
