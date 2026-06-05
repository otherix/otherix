// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"fmt"
	"net/http"
)

// InjectedError is the typed payload InjectError consumes.
type InjectedError struct {
	Status  int            // HTTP status (4xx / 5xx).
	Code    string         // Envelope code; must be in agent.yaml ErrorCode.
	Message string         // Human-readable, en-US.
	Details map[string]any // Optional details.
}

// RequestAction short-circuits a request from OnRequest. Status and
// Body are written verbatim; Header is merged into the response.
type RequestAction struct {
	Status int
	Body   any // JSON-marshalled.
	Header http.Header
}

// OnRequestFunc is the callback signature OnRequest accepts. It runs
// after the route is matched and before the handler body. A non-nil
// return short-circuits the handler with the action; nil delegates
// to the regular handler.
type OnRequestFunc func(r *http.Request, operationID string) *RequestAction

// injectionState bundles the three fault-injection layers used by
// handlers: per-operationID single-shot, per-operationID persistent,
// and a single OnRequest callback.
type injectionState struct {
	single     map[string]InjectedError
	persistent map[string]InjectedError
	onRequest  OnRequestFunc
}

// operationIDs enumerates every operationId declared in
// api/openapi/agent.yaml. The set is consumed by InjectError and
// InjectErrorPersistent for input validation; a drift between this
// map and agentapi.ServerInterface is caught by
// TestInject_OperationIDsMatchServerInterface.
var operationIDs = map[string]struct{}{
	"health.check":               {},
	"info.get":                   {},
	"networks.list":              {},
	"networks.configure":         {},
	"networks.get":               {},
	"networks.delete":            {},
	"storagePools.list":          {},
	"storagePools.get":           {},
	"storagePools.scan":          {},
	"tasks.get":                  {},
	"vms.list":                   {},
	"vms.create":                 {},
	"vms.get":                    {},
	"vms.delete":                 {},
	"vms.start":                  {},
	"vms.stop":                   {},
	"vms.poweroff":               {},
	"vms.reboot":                 {},
	"vms.reset":                  {},
	"vms.pause":                  {},
	"vms.resume":                 {},
	"vms.resize":                 {},
	"vms.revert":                 {},
	"vmDisks.list":               {},
	"vmDisks.attach":             {},
	"vmDisks.detach":             {},
	"vmDisks.update":             {},
	"vmNics.list":                {},
	"vmNics.attach":              {},
	"vmNics.detach":              {},
	"vmNics.update":              {},
	"vmSnapshots.list":           {},
	"vmSnapshots.create":         {},
	"vmSnapshots.get":            {},
	"vmSnapshots.delete":         {},
	"vmMigrations.startIncoming": {},
	"vmMigrations.startOutgoing": {},
	"vmMigrations.get":           {},
	"vmMigrations.cancel":        {},
	"vmConsole.issueToken":       {},
	"vmConsole.stream":           {},
	"vms.logs":                   {},
}

// InjectError makes the next call to operationID return err as the
// agent.yaml error envelope. Consumed once. Panics when operationID
// is not an agent.yaml operation — a misuse the test author should
// see immediately rather than silently fail to inject.
func (m *Mock) InjectError(operationID string, err InjectedError) {
	requireKnownOperation(operationID)
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.inject.single == nil {
		m.state.inject.single = map[string]InjectedError{}
	}
	m.state.inject.single[operationID] = err
}

// InjectErrorPersistent installs an injection that fires on every
// call to operationID until cleared. Replaces any prior persistent
// injection for the same id.
func (m *Mock) InjectErrorPersistent(operationID string, err InjectedError) {
	requireKnownOperation(operationID)
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.inject.persistent == nil {
		m.state.inject.persistent = map[string]InjectedError{}
	}
	m.state.inject.persistent[operationID] = err
}

// ClearInjections removes every installed error injection and the
// OnRequest callback.
func (m *Mock) ClearInjections() {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.inject.single = nil
	m.state.inject.persistent = nil
	m.state.inject.onRequest = nil
}

// OnRequest installs a low-level callback that runs after route match
// and before the handler. A nil fn clears the callback. Single-slot
// — installing replaces the previous.
func (m *Mock) OnRequest(fn OnRequestFunc) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.inject.onRequest = fn
}

func requireKnownOperation(operationID string) {
	if _, ok := operationIDs[operationID]; !ok {
		panic(fmt.Sprintf("agentmock: unknown operationID %q", operationID))
	}
}

// takeInjectionLocked returns and consumes the matching injection
// for operationID, or zero/false when none is installed. Persistent
// injections are returned without consumption. Caller holds mu.
func (s *state) takeInjectionLocked(operationID string) (InjectedError, bool) {
	if e, ok := s.inject.single[operationID]; ok {
		delete(s.inject.single, operationID)
		return e, true
	}
	if e, ok := s.inject.persistent[operationID]; ok {
		return e, true
	}
	return InjectedError{}, false
}
