// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import "context"

// FakeResponder is a Spy implementation of Responder for tests. It records
// every call into an exported slice and returns a configurable error per
// method via Errs, keyed by method name (nil or absent => success).
// FakeResponder is not safe for concurrent use.
type FakeResponder struct {
	// Errs maps a method name ("RegisterNetwork"/"DeregisterNetwork") to the
	// error that method returns. A nil or absent entry means success.
	Errs map[string]error

	RegisterCalls   []NetworkConfig
	DeregisterCalls []string
}

func (f *FakeResponder) err(method string) error {
	if f.Errs == nil {
		return nil
	}
	return f.Errs[method]
}

// RegisterNetwork records cfg and returns Errs["RegisterNetwork"].
func (f *FakeResponder) RegisterNetwork(cfg NetworkConfig) error {
	f.RegisterCalls = append(f.RegisterCalls, cfg)
	return f.err("RegisterNetwork")
}

// DeregisterNetwork records networkID and returns Errs["DeregisterNetwork"].
func (f *FakeResponder) DeregisterNetwork(networkID string) error {
	f.DeregisterCalls = append(f.DeregisterCalls, networkID)
	return f.err("DeregisterNetwork")
}

// Run blocks until ctx is cancelled and returns ctx.Err(). The Spy serves no
// traffic; it exists so FakeResponder satisfies Responder.
func (f *FakeResponder) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Ensure FakeResponder satisfies Responder at compile time.
var _ Responder = (*FakeResponder)(nil)
