// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package lb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/lb"
)

// runLbCmd executes the `lb` cobra subcommand tree against args, mounting
// it on a throwaway parent that exposes the same persistent flags the real
// root provides. Parallel of cmd/cli/network_test.runNetworkCmd.
func runLbCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := lb.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	full := append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...)
	parent.SetArgs(full)
	parent.SilenceUsage = true
	parent.SilenceErrors = true
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

// TestLbCreate_HealthCheckBody asserts `lb create` sends a health_check
// object carrying only the sub-fields the operator set (--health-port,
// --health-interval); the unset sub-fields are absent so the server applies
// its default for each.
func TestLbCreate_HealthCheckBody(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/loadbalancers" || r.Method != http.MethodPost {
			t.Errorf("%s %s, want POST /v1/loadbalancers", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       uuid.NewString(),
			"name":     "web",
			"owner_id": uuid.NewString(),
			"port":     80,
			"selector": map[string]any{"app": "web"},
			"health_check": map[string]any{
				"port":                8080,
				"interval_seconds":    5,
				"timeout_seconds":     2,
				"healthy_threshold":   2,
				"unhealthy_threshold": 3,
			},
			"backends":   []any{},
			"created_at": "2026-07-01T10:00:00Z",
			"updated_at": "2026-07-01T10:00:00Z",
		})
	}))
	defer srv.Close()

	_, _, err := runLbCmd(t, srv.URL, []string{
		"create", "web", "--port", "80", "--selector", "app=web",
		"--health-port", "8080", "--health-interval", "5",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	hc, ok := gotBody["health_check"].(map[string]any)
	if !ok {
		t.Fatalf("health_check missing or not an object: %#v", gotBody["health_check"])
	}
	want := map[string]any{
		"port":             float64(8080),
		"interval_seconds": float64(5),
	}
	if diff := cmp.Diff(want, hc); diff != "" {
		t.Errorf("health_check body mismatch (-want +got):\n%s", diff)
	}
}

// TestLbCreate_NoHealthFlagsOmitsBlock asserts that when no --health-*
// flag is set the create body carries no health_check object at all, so
// the server keeps its full default health-check config.
func TestLbCreate_NoHealthFlagsOmitsBlock(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         uuid.NewString(),
			"name":       "web",
			"owner_id":   uuid.NewString(),
			"port":       80,
			"selector":   map[string]any{"app": "web"},
			"backends":   []any{},
			"created_at": "2026-07-01T10:00:00Z",
			"updated_at": "2026-07-01T10:00:00Z",
		})
	}))
	defer srv.Close()

	if _, _, err := runLbCmd(t, srv.URL, []string{
		"create", "web", "--port", "80", "--selector", "app=web",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, present := gotBody["health_check"]; present {
		t.Errorf("health_check present with no --health-* flags: %#v", gotBody["health_check"])
	}
}

// TestLbCreate_HealthPortZeroOmitted asserts that an explicit --health-port 0
// (the follow-the-traffic-port sentinel) never reaches the wire as port:0, which
// would violate the OpenAPI minimum:1. With --health-port 0 the only flag, the
// whole health_check block is omitted; with another --health-* flag alongside,
// health_check is present but carries no port key.
func TestLbCreate_HealthPortZeroOmitted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		args          []string
		wantHCPresent bool
	}{
		{
			name:          "port-zero-only omits the block",
			args:          []string{"--health-port", "0"},
			wantHCPresent: false,
		},
		{
			name:          "port-zero with another flag omits only port",
			args:          []string{"--health-port", "0", "--health-interval", "5"},
			wantHCPresent: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": uuid.NewString(), "name": "web", "owner_id": uuid.NewString(),
					"port": 80, "selector": map[string]any{"app": "web"}, "backends": []any{},
					"created_at": "2026-07-01T10:00:00Z", "updated_at": "2026-07-01T10:00:00Z",
				})
			}))
			defer srv.Close()

			args := append([]string{"create", "web", "--port", "80", "--selector", "app=web"}, tc.args...)
			if _, _, err := runLbCmd(t, srv.URL, args); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			hc, present := gotBody["health_check"]
			if present != tc.wantHCPresent {
				t.Fatalf("health_check present = %v, want %v; body=%#v", present, tc.wantHCPresent, gotBody)
			}
			if present {
				m, ok := hc.(map[string]any)
				if !ok {
					t.Fatalf("health_check not an object: %#v", hc)
				}
				if _, hasPort := m["port"]; hasPort {
					t.Errorf("health_check carries a port key on explicit --health-port 0: %#v", m)
				}
			}
		})
	}
}
