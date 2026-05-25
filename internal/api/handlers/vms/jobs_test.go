// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/agentclient"
)

// TestClassifyVMError covers the executor-error → tasks.error.code
// mapping. Agent envelopes pass through verbatim; TimeoutError
// collapses to request_timeout; everything else falls back to the
// caller-supplied bucket.
func TestClassifyVMError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		fallback string
		want     string
	}{
		{
			name:     "agent error with code → passthrough",
			err:      &agentclient.AgentError{Status: 500, Code: "qemu_spawn_failed"},
			fallback: errCodeVMCreateFailed,
			want:     "qemu_spawn_failed",
		},
		{
			name:     "agent error without code → agent_unreachable",
			err:      &agentclient.AgentError{Status: 502},
			fallback: errCodeVMCreateFailed,
			want:     errCodeVMAgentUnreachabl,
		},
		{
			name:     "timeout error → request_timeout",
			err:      &agentclient.TimeoutError{},
			fallback: errCodeVMCreateFailed,
			want:     errCodeVMTimeout,
		},
		{
			name:     "unrecognised → fallback create",
			err:      errors.New("kaboom"),
			fallback: errCodeVMCreateFailed,
			want:     errCodeVMCreateFailed,
		},
		{
			name:     "unrecognised → fallback delete",
			err:      errors.New("kaboom"),
			fallback: errCodeVMDeleteFailed,
			want:     errCodeVMDeleteFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyVMError(tc.err, tc.fallback)
			if got != tc.want {
				t.Errorf("classifyVMError = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyLoadError covers the pgx.ErrNoRows → not_found path
// and the "anything else" → internal fallback.
func TestClassifyLoadError(t *testing.T) {
	t.Parallel()
	if got := classifyLoadError(pgx.ErrNoRows, errCodeVMNotFound); got != errCodeVMNotFound {
		t.Errorf("ErrNoRows = %q, want %q", got, errCodeVMNotFound)
	}
	if got := classifyLoadError(errors.New("other"), errCodeVMNotFound); got != "internal" {
		t.Errorf("non-ErrNoRows = %q, want internal", got)
	}
}
