// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobbroker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// storeStub satisfies the broker's Store seam. It records every phase the
// broker drives so the test can assert the saga lifecycle alongside the agent
// call order.
type storeStub struct {
	holders       []uuid.UUID
	imageHolders  []uuid.UUID
	nodeEndp      map[uuid.UUID]string
	nodeName      map[uuid.UUID]string
	phases        []store.PullSagaPhase
	token         string
	blobSize      int64
	imageBlobSize int64
}

func (s *storeStub) BlobHolders(_ context.Context, _ string) ([]uuid.UUID, error) {
	return s.holders, nil
}

func (s *storeStub) ImageBlobHolders(_ context.Context, _ string) ([]uuid.UUID, error) {
	return s.imageHolders, nil
}

func (s *storeStub) BlobSize(_ context.Context, _ string) (int64, bool) {
	return s.blobSize, s.blobSize > 0
}

func (s *storeStub) ImageBlobSize(_ context.Context, _ string) (int64, bool) {
	return s.imageBlobSize, s.imageBlobSize > 0
}

func (s *storeStub) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	return store.Node{ID: id, Name: s.nodeName[id], AdvertisedEndpoint: s.nodeEndp[id]}, nil
}

func (s *storeStub) CreatePullSaga(_ context.Context, p store.CreatePullSagaParams) (store.ArtifactPullSaga, string, error) {
	return store.ArtifactPullSaga{ID: p.ID, Digest: p.Digest}, s.token, nil
}

func (s *storeStub) UpdatePullSagaServeEndpoint(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (s *storeStub) SetPullSagaPhase(_ context.Context, _ uuid.UUID, phase store.PullSagaPhase) error {
	s.phases = append(s.phases, phase)
	return nil
}

// agentSpy records the order of serve/pull/stop calls (the seam under test).
type agentSpy struct {
	calls          []string
	serveEndp      string
	pullHolderIdty string
	pullExpSize    int64
	pullTier       Tier
	stopToken      string
}

func (a *agentSpy) ServeBlob(_ context.Context, holderEndpoint, _, _, _ string) (string, string, error) {
	a.calls = append(a.calls, "serve:"+holderEndpoint)
	return a.serveEndp, "2030-01-01T00:00:00Z", nil
}

func (a *agentSpy) PullBlobAndAwait(_ context.Context, consumerEndpoint, _, _, holderEndpoint, holderIdentity string, expectedSize int64, tier Tier) error {
	a.calls = append(a.calls, "pull:"+consumerEndpoint+"<-"+holderEndpoint)
	a.pullHolderIdty = holderIdentity
	a.pullExpSize = expectedSize
	a.pullTier = tier
	return nil
}

func (a *agentSpy) StopServe(_ context.Context, holderEndpoint, token string) error {
	a.calls = append(a.calls, "stop:"+holderEndpoint)
	a.stopToken = token
	return nil
}

func TestBrokerPullSequencing(t *testing.T) {
	holder, consumer := uuid.New(), uuid.New()
	st := &storeStub{
		holders:  []uuid.UUID{holder},
		nodeEndp: map[uuid.UUID]string{holder: "https://holder:9443", consumer: "https://consumer:9443"},
		nodeName: map[uuid.UUID]string{holder: "node-1", consumer: "node-2"},
		token:    "otx_pull_x",
		blobSize: 65536,
	}
	spy := &agentSpy{serveEndp: "https://holder:49252"}
	b := New(st, spy, testLogger())

	if err := b.BrokerPull(context.Background(), "abc", consumer, TierArtifact); err != nil {
		t.Fatalf("BrokerPull: %v", err)
	}

	want := []string{
		"serve:https://holder:9443",
		"pull:https://consumer:9443<-https://holder:49252",
		"stop:https://holder:9443",
	}
	if len(spy.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", spy.calls, want)
	}
	for i := range want {
		if spy.calls[i] != want[i] {
			t.Errorf("call[%d] = %q, want %q", i, spy.calls[i], want[i])
		}
	}

	// The consumer pull pins TLS to the holder's node identity SAN, derived
	// from the holder node name (mirrors migration target_node_identity).
	if want := "node-node-1.agents.otherix.local"; spy.pullHolderIdty != want {
		t.Errorf("pull holder identity = %q, want %q", spy.pullHolderIdty, want)
	}

	// The CP-known blob size (from observed inventory) flows to the consumer so
	// it can bound the pull body.
	if spy.pullExpSize != 65536 {
		t.Errorf("pull expected size = %d, want 65536", spy.pullExpSize)
	}

	// The serve teardown is keyed on the per-op token minted by CreatePullSaga,
	// not the digest - so it names exactly this serve even if another serve of
	// the same digest is concurrently live to a different consumer.
	if spy.stopToken != st.token {
		t.Errorf("stop serve token = %q, want %q (the per-op token, not the digest)", spy.stopToken, st.token)
	}

	// The saga must reach complete via serving (UpdatePullSagaServeEndpoint
	// sets serving itself) -> pulling -> complete.
	wantPhases := []store.PullSagaPhase{store.PullSagaPhasePulling, store.PullSagaPhaseComplete}
	if len(st.phases) != len(wantPhases) {
		t.Fatalf("phases = %v, want %v", st.phases, wantPhases)
	}
	for i := range wantPhases {
		if st.phases[i] != wantPhases[i] {
			t.Errorf("phase[%d] = %q, want %q", i, st.phases[i], wantPhases[i])
		}
	}
}

func TestBrokerPullNoHolderFailsClosed(t *testing.T) {
	consumer := uuid.New()
	st := &storeStub{holders: nil}
	spy := &agentSpy{}
	b := New(st, spy, testLogger())

	err := b.BrokerPull(context.Background(), "abc", consumer, TierArtifact)
	if !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("BrokerPull no-holder err = %v, want ErrBlobUnavailable", err)
	}
	if len(spy.calls) != 0 {
		t.Errorf("agent contacted despite no holder: %v", spy.calls)
	}
}

func TestBrokerPullImageTierUsesImageHolders(t *testing.T) {
	holder, consumer := uuid.New(), uuid.New()
	st := &storeStub{
		// Image-tier holder present; the artifact holder source is EMPTY, so a
		// successful pull proves the broker discovered holders from the image tier.
		holders:      nil,
		imageHolders: []uuid.UUID{holder},
		nodeEndp:     map[uuid.UUID]string{holder: "https://holder:9443", consumer: "https://consumer:9443"},
		nodeName:     map[uuid.UUID]string{holder: "node-1", consumer: "node-2"},
		token:        "otx_pull_x",
		// The artifact-tier size returns 0 for an image-only digest; the image-tier
		// size is the authoritative bound. The broker must read the image tier so
		// the pull is capped rather than running uncapped.
		blobSize:      0,
		imageBlobSize: 4096,
	}
	spy := &agentSpy{serveEndp: "https://holder:49252"}
	b := New(st, spy, testLogger())

	if err := b.BrokerPull(context.Background(), "abc", consumer, TierImage); err != nil {
		t.Fatalf("BrokerPull image tier: %v", err)
	}
	if spy.pullTier != TierImage {
		t.Errorf("pull tier = %q, want %q", spy.pullTier, TierImage)
	}
	// The size bound on the image-tier pull must come from the image inventory,
	// not the artifact inventory (which returns 0 for an image-only digest).
	if spy.pullExpSize != 4096 {
		t.Errorf("image-tier pull expected size = %d, want 4096 (from image inventory)", spy.pullExpSize)
	}
}

func TestBrokerPullArtifactTierUnchanged(t *testing.T) {
	holder, consumer := uuid.New(), uuid.New()
	st := &storeStub{
		// Artifact-tier holder present; the image holder source is EMPTY, so a
		// successful pull proves the broker still discovers from the artifact tier.
		holders:      []uuid.UUID{holder},
		imageHolders: nil,
		nodeEndp:     map[uuid.UUID]string{holder: "https://holder:9443", consumer: "https://consumer:9443"},
		nodeName:     map[uuid.UUID]string{holder: "node-1", consumer: "node-2"},
		token:        "otx_pull_x",
	}
	spy := &agentSpy{serveEndp: "https://holder:49252"}
	b := New(st, spy, testLogger())

	if err := b.BrokerPull(context.Background(), "abc", consumer, TierArtifact); err != nil {
		t.Fatalf("BrokerPull artifact tier: %v", err)
	}
	if spy.pullTier != TierArtifact {
		t.Errorf("pull tier = %q, want %q", spy.pullTier, TierArtifact)
	}
}
