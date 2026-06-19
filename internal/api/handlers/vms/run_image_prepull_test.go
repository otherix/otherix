// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/blobbroker"
)

// imagePrepullStoreStub serves a settable ImageBlobHolders result (and records
// whether the lookup ran) so the image cache pre-pull tests can drive the
// fail-open seam directly. holdersErr, when set, models an inventory read error.
// It embeds createLifecycleWorkerStoreStub so it satisfies the full WorkerStore
// surface ensureImageBlobLocal's parameter type requires.
type imagePrepullStoreStub struct {
	createLifecycleWorkerStoreStub
	holders    []uuid.UUID
	holdersErr error

	holdersCalled bool
}

func (s *imagePrepullStoreStub) ImageBlobHolders(context.Context, string) ([]uuid.UUID, error) {
	s.holdersCalled = true
	return s.holders, s.holdersErr
}

// imagePrepullBrokerSpy records BrokerPull calls so the tests can assert the
// pull fired (or not) with the expected digest/node/tier, and surfaces a canned
// error to prove the swallow path.
type imagePrepullBrokerSpy struct {
	err error

	calls []imagePrepullCall
}

type imagePrepullCall struct {
	digest string
	node   uuid.UUID
	tier   blobbroker.Tier
}

func (b *imagePrepullBrokerSpy) BrokerPull(_ context.Context, digest string, node uuid.UUID, tier blobbroker.Tier) error {
	b.calls = append(b.calls, imagePrepullCall{digest: digest, node: node, tier: tier})
	return b.err
}

func TestEnsureImageBlobLocalBrokersWhenPeerHolds(t *testing.T) {
	target := uuid.New()
	peer := uuid.New()
	st := &imagePrepullStoreStub{holders: []uuid.UUID{peer}}
	broker := &imagePrepullBrokerSpy{}

	ensureImageBlobLocal(context.Background(), st, broker, target, "deadbeef", discardLog())

	if len(broker.calls) != 1 {
		t.Fatalf("BrokerPull calls = %d, want 1", len(broker.calls))
	}
	got := broker.calls[0]
	if got.digest != "deadbeef" || got.node != target || got.tier != blobbroker.TierImage {
		t.Errorf("BrokerPull(%q, %v, %q), want (deadbeef, %v, %q)",
			got.digest, got.node, got.tier, target, blobbroker.TierImage)
	}
}

func TestEnsureImageBlobLocalSkipsWhenNoPeer(t *testing.T) {
	target := uuid.New()
	st := &imagePrepullStoreStub{holders: nil}
	broker := &imagePrepullBrokerSpy{}

	ensureImageBlobLocal(context.Background(), st, broker, target, "deadbeef", discardLog())

	if !st.holdersCalled {
		t.Errorf("ImageBlobHolders was not consulted")
	}
	if len(broker.calls) != 0 {
		t.Errorf("BrokerPull calls = %d, want 0 (no peer holder)", len(broker.calls))
	}
}

func TestEnsureImageBlobLocalSkipsWhenTargetAlreadyHolds(t *testing.T) {
	target := uuid.New()
	st := &imagePrepullStoreStub{holders: []uuid.UUID{target}}
	broker := &imagePrepullBrokerSpy{}

	ensureImageBlobLocal(context.Background(), st, broker, target, "deadbeef", discardLog())

	if len(broker.calls) != 0 {
		t.Errorf("BrokerPull calls = %d, want 0 (target already holds)", len(broker.calls))
	}
}

func TestEnsureImageBlobLocalSwallowsHolderLookupError(t *testing.T) {
	target := uuid.New()
	st := &imagePrepullStoreStub{holdersErr: errors.New("etcd down")}
	broker := &imagePrepullBrokerSpy{}

	// Must not panic and must not call the broker (FAIL-OPEN).
	ensureImageBlobLocal(context.Background(), st, broker, target, "deadbeef", discardLog())

	if len(broker.calls) != 0 {
		t.Errorf("BrokerPull calls = %d, want 0 (lookup error swallowed)", len(broker.calls))
	}
}

func TestEnsureImageBlobLocalSwallowsBrokerError(t *testing.T) {
	target := uuid.New()
	peer := uuid.New()
	st := &imagePrepullStoreStub{holders: []uuid.UUID{peer}}
	broker := &imagePrepullBrokerSpy{err: errors.New("boom")}

	// Must return cleanly even though the broker errored (create proceeds).
	ensureImageBlobLocal(context.Background(), st, broker, target, "deadbeef", discardLog())

	if len(broker.calls) != 1 {
		t.Errorf("BrokerPull calls = %d, want 1 (broker was attempted)", len(broker.calls))
	}
}

func TestEnsureImageBlobLocalSkipsEmptyDigest(t *testing.T) {
	target := uuid.New()
	st := &imagePrepullStoreStub{holders: []uuid.UUID{uuid.New()}}
	broker := &imagePrepullBrokerSpy{}

	ensureImageBlobLocal(context.Background(), st, broker, target, "", discardLog())

	if st.holdersCalled {
		t.Errorf("ImageBlobHolders consulted for an empty digest; want skipped")
	}
	if len(broker.calls) != 0 {
		t.Errorf("BrokerPull calls = %d, want 0 (empty digest)", len(broker.calls))
	}
}
