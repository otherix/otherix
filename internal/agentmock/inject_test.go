// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/agentapi"
)

func TestInject_OperationIDsMatchServerInterface(t *testing.T) {
	iface := reflect.TypeFor[agentapi.ServerInterface]()
	if got := iface.NumMethod(); got != len(operationIDs) {
		t.Fatalf("ServerInterface has %d methods, operationIDs map has %d — drift",
			got, len(operationIDs))
	}
	for opID := range operationIDs {
		method := operationIDToMethodName(opID)
		if _, ok := iface.MethodByName(method); !ok {
			t.Errorf("operationID %q maps to method %q, not present on ServerInterface",
				opID, method)
		}
	}
}

// operationIDToMethodName mirrors the codegen rule oapi-codegen
// applies with ToCamelCaseWithInitialisms: split on '.', title-case
// each segment, then apply the project's additional-initialisms
// (VM, QMP, QEMU, VNC, SPICE, SHA, CSR). Keep this function in the
// test file rather than the package code — it exists only to
// validate the static operationIDs map.
func operationIDToMethodName(opID string) string {
	parts := strings.Split(opID, ".")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	joined := strings.Join(parts, "")
	// Mirror the oapi-codegen initialism overrides from
	// api/openapi/oapi-codegen.yaml. Order matters: longer
	// prefixes first так не collide с shorter ones.
	initialismPrefixes := []struct{ from, to string }{
		{"Vm", "VM"},
	}
	for _, ip := range initialismPrefixes {
		if strings.HasPrefix(joined, ip.from) && len(joined) > len(ip.from) {
			next := joined[len(ip.from)]
			if next >= 'A' && next <= 'Z' {
				joined = ip.to + joined[len(ip.from):]
			}
		}
	}
	return joined
}

func TestInjectError_SingleShotConsumed(t *testing.T) {
	m := Start(t, Options{})
	m.InjectError("health.check", InjectedError{
		Status:  http.StatusServiceUnavailable,
		Code:    "internal",
		Message: "boom",
	})

	resp1 := mustGet(t, m.URL()+"/v1/health")
	if resp1.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("first call status = %d, want 503", resp1.StatusCode)
	}
	resp2 := mustGet(t, m.URL()+"/v1/health")
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second call status = %d, want 200 (injection consumed)", resp2.StatusCode)
	}
}

func TestInjectErrorPersistent_StickyUntilCleared(t *testing.T) {
	m := Start(t, Options{})
	m.InjectErrorPersistent("health.check", InjectedError{
		Status:  http.StatusInternalServerError,
		Code:    "internal",
		Message: "always",
	})

	for i := range 3 {
		resp := mustGet(t, m.URL()+"/v1/health")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("call %d status = %d, want 500", i, resp.StatusCode)
		}
	}

	m.ClearInjections()
	resp := mustGet(t, m.URL()+"/v1/health")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after ClearInjections status = %d, want 200", resp.StatusCode)
	}
}

func TestInject_UnknownOperationIDPanics(t *testing.T) {
	m := Start(t, Options{})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on unknown operationID")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "no.such.op") {
			t.Errorf("panic message = %q, want substring %q", msg, "no.such.op")
		}
	}()
	m.InjectError("no.such.op", InjectedError{Status: 500, Code: "internal", Message: "x"})
}

func TestOnRequest_FiresWithOperationID(t *testing.T) {
	m := Start(t, Options{})

	type capture struct {
		opID string
	}
	got := capture{}
	m.OnRequest(func(_ *http.Request, opID string) *RequestAction {
		got.opID = opID
		return &RequestAction{
			Status: http.StatusTeapot,
			Body:   map[string]string{"hello": "world"},
		}
	})

	resp := mustGet(t, m.URL()+"/v1/health")
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418 from OnRequest action", resp.StatusCode)
	}
	if got.opID != "health.check" {
		t.Errorf("OnRequest opID = %q, want %q", got.opID, "health.check")
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parsed map[string]string
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if parsed["hello"] != "world" {
		t.Errorf("body[hello] = %q, want world", parsed["hello"])
	}
}

func mustGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // tests use httptest URLs.
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
