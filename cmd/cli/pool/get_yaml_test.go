// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPoolGetOutputYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":               "default",
			"type":               "local_dir",
			"is_cluster_default": true,
			"instances": []map[string]any{
				{
					"id":                 uuid.NewString(),
					"node":               "node-a",
					"name":               "default",
					"type":               "local_dir",
					"path":               "/opt/otherix/pools/default",
					"is_cluster_default": true,
				},
				{
					"id":                 uuid.NewString(),
					"node":               "node-b",
					"name":               "default",
					"type":               "local_dir",
					"path":               "/opt/otherix/pools/default",
					"is_cluster_default": true,
				},
			},
		})
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{"get", "default", "-o", "yaml"})
	if err != nil {
		t.Fatalf("get -o yaml error = %v", err)
	}
	for _, want := range []string{"apiVersion: otherix/v1", "kind: StoragePool", "name: default", "nodeList", "node-a", "node-b"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("yaml output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "is_cluster_default") || strings.Contains(stdout, "id:") {
		t.Errorf("yaml output leaked server fields:\n%s", stdout)
	}
}
