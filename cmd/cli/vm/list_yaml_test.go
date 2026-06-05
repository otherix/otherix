// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestVMListOutputYAML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Two VMs using the real cpclient.VM json tags (vms.go). ProjectVM
		// needs name, image_url, architecture, vcpus, memory_mb; the rest
		// decode tolerantly.
		_, _ = w.Write([]byte(`{"data":[` +
			`{"id":"` + uuid.NewString() + `","name":"web-1","image_url":"https://x/u.qcow2","architecture":"amd64","vcpus":2,"memory_mb":512},` +
			`{"id":"` + uuid.NewString() + `","name":"web-2","image_url":"https://x/v.qcow2","architecture":"arm64","vcpus":1,"memory_mb":256}` +
			`],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()
	stdout, _, err := runVMCmd(t, srv.URL, []string{"list", "-o", "yaml"})
	if err != nil {
		t.Fatalf("vm list -o yaml error = %v", err)
	}
	if got := strings.Count(stdout, "kind: VM"); got != 2 {
		t.Errorf("kind: VM count = %d, want 2:\n%s", got, stdout)
	}
	if strings.Count(stdout, "---") != 1 {
		t.Errorf("want exactly 1 separator:\n%s", stdout)
	}
}
