// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPoolListOutputYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[` +
			`{"id":"` + uuid.NewString() + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p"},` +
			`{"id":"` + uuid.NewString() + `","name":"p2","node":"node-2","type":"local_dir","path":"/opt/q"}` +
			`],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()
	stdout, _, err := runPoolCmd(t, srv.URL, []string{"list", "-o", "yaml"})
	if err != nil {
		t.Fatalf("pool list -o yaml error = %v", err)
	}
	if got := strings.Count(stdout, "kind: StoragePool"); got != 2 {
		t.Errorf("kind: StoragePool count = %d, want 2:\n%s", got, stdout)
	}
	if got := strings.Count(stdout, "---"); got != 1 {
		t.Errorf("separator count = %d, want 1:\n%s", got, stdout)
	}
	if strings.Contains(stdout, "status") || strings.Contains(stdout, "id:") {
		t.Errorf("server fields leaked:\n%s", stdout)
	}
}
