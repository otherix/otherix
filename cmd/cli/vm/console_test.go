// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestClassifyConsoleDialError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		resp           *http.Response
		dialErr        error
		wantSubstrings []string
		notSubstrings  []string
	}{
		{
			name:           "nil response surfaces connect_failed",
			resp:           nil,
			dialErr:        errors.New("dial tcp: connection refused"),
			wantSubstrings: []string{"connect_failed", "connection refused", "demo"},
			notSubstrings:  []string{"websocket", "handshake", "101"},
		},
		{
			name: "409 console_in_use surfaces friendly message",
			resp: responseWithBody(http.StatusConflict,
				`{"error":{"code":"console_in_use","message":"another console session is already open for this vm"}}`),
			wantSubstrings: []string{"console_in_use", "another console session", "Ctrl+]", "and retry"},
			notSubstrings:  []string{"websocket", "handshake", "101", "409"},
		},
		{
			name: "409 vm_not_running suggests start",
			resp: responseWithBody(http.StatusConflict,
				`{"error":{"code":"vm_not_running","message":"vm is not running"}}`),
			wantSubstrings: []string{"vm_not_running", "is not running", "otherix vm start"},
			notSubstrings:  []string{"websocket", "101"},
		},
		{
			name: "409 protocol_not_available is explicit",
			resp: responseWithBody(http.StatusConflict,
				`{"error":{"code":"protocol_not_available","message":"vm does not expose serial"}}`),
			wantSubstrings: []string{"protocol_not_available", "serial console"},
		},
		{
			name: "401 token rejection suggests retry",
			resp: responseWithBody(http.StatusUnauthorized,
				`{"error":{"code":"unauthenticated","message":"invalid or expired token"}}`),
			wantSubstrings: []string{"unauthenticated", "rejected", "retry"},
			notSubstrings:  []string{"websocket", "401"},
		},
		{
			name: "403 permission denied is operator-language",
			resp: responseWithBody(http.StatusForbidden,
				`{"error":{"code":"permission_denied","message":"insufficient permission"}}`),
			wantSubstrings: []string{"permission_denied", "do not have permission"},
		},
		{
			name: "404 vm not found",
			resp: responseWithBody(http.StatusNotFound,
				`{"error":{"code":"vm_not_found","message":"vm not found"}}`),
			wantSubstrings: []string{"vm_not_found", "demo"},
		},
		{
			name:           "502 agent unreachable is named",
			resp:           responseWithBody(http.StatusBadGateway, `{"error":{"code":"agent_unreachable","message":"agent unreachable"}}`),
			wantSubstrings: []string{"agent_unreachable", "agent owning"},
		},
		{
			name:           "409 without envelope falls back to conflict",
			resp:           responseWithBody(http.StatusConflict, ``),
			wantSubstrings: []string{"conflict", "demo"},
			notSubstrings:  []string{"websocket"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyConsoleDialError("demo", tc.resp, tc.dialErr)
			msg := got.Error()
			for _, sub := range tc.wantSubstrings {
				if !strings.Contains(msg, sub) {
					t.Errorf("error %q missing wanted substring %q", msg, sub)
				}
			}
			for _, sub := range tc.notSubstrings {
				if strings.Contains(msg, sub) {
					t.Errorf("error %q contains forbidden substring %q (leaks transport detail)", msg, sub)
				}
			}
			// Hard rule: operator-facing strings stay ASCII. Cyrillic
			// look-alikes (а, е, о, и, ...) are easy to type when
			// editing mixed-language source, but they read as gibberish
			// when copied into a bug report or ticket title.
			for _, r := range msg {
				if r > 0x7E {
					t.Errorf("error %q contains non-ASCII rune %q (U+%04X)", msg, r, r)
				}
			}
		})
	}
}

func TestConsoleMigratedCloseHint(t *testing.T) {
	t.Parallel()
	if got := consoleCloseHint(consoleMigratedCloseCodeCLI); got == "" {
		t.Errorf("consoleCloseHint(4002) = empty, want a reconnect hint")
	}
	if got := consoleCloseHint(websocket.StatusNormalClosure); got != "" {
		t.Errorf("consoleCloseHint(normal) = %q, want empty", got)
	}
}

func responseWithBody(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
