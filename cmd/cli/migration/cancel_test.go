// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/migration"
)

// runMigrationCmd executes the `otherix migration ...` subtree against endpoint
// with a stub token, returning captured stdout/stderr and the execute error.
func runMigrationCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := migration.NewCommand()
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

// TestMigrationCancel_Happy covers the wiring: `migration cancel <id>` POSTs to
// /v1/migrations/{id}/cancel and prints the resulting phase from the 200 body.
func TestMigrationCancel_Happy(t *testing.T) {
	t.Parallel()
	const id = "33333333-3333-3333-3333-333333333333"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/migrations/"+id+"/cancel" {
			t.Errorf("path = %s, want /v1/migrations/%s/cancel", r.URL.Path, id)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":               id,
			"vm_id":            "44444444-4444-4444-4444-444444444444",
			"reason":           "manual",
			"phase":            "cancelled",
			"progress_percent": 0,
			"live":             true,
			"target_pool_name": "",
			"created_at":       "2026-06-14T10:00:00Z",
			"updated_at":       "2026-06-14T10:00:02Z",
		})
	}))
	defer srv.Close()

	stdout, _, err := runMigrationCmd(t, srv.URL, []string{"cancel", id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, "migration "+id+": cancelled") {
		t.Errorf("stdout missing cancelled phase line: %q", stdout)
	}
}

// TestMigrationCancel_NotFound covers the 404 path: the command surfaces the
// server error instead of printing a success line.
func TestMigrationCancel_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"migration not found"}}`))
	}))
	defer srv.Close()

	_, _, err := runMigrationCmd(t, srv.URL, []string{"cancel", "missing-id"})
	if err == nil {
		t.Fatal("expected error for 404 cancel")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want mention of not_found", err)
	}
}
