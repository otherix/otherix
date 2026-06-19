// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

type replicateStoreSpy struct {
	alreadyTerminal bool
	finalized       []store.UpdateTaskFinalizedParams
}

func (s *replicateStoreSpy) UpdateTaskRunning(_ context.Context, _ uuid.UUID) (bool, error) {
	return s.alreadyTerminal, nil
}

func (s *replicateStoreSpy) UpdateTaskFinalized(_ context.Context, p store.UpdateTaskFinalizedParams) error {
	s.finalized = append(s.finalized, p)
	return nil
}

type brokerStub struct {
	gotDigest string
	gotNode   uuid.UUID
	err       error
}

func (b *brokerStub) BrokerPull(_ context.Context, digest string, node uuid.UUID) error {
	b.gotDigest, b.gotNode = digest, node
	return b.err
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustJSON(t *testing.T, v ReplicateArgs) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestReplicateHandlerSuccess(t *testing.T) {
	st := &replicateStoreSpy{}
	br := &brokerStub{}
	tid, target := uuid.New(), uuid.New()
	args := mustJSON(t, ReplicateArgs{TaskID: tid, Digest: "d0", TargetNodeID: target})

	if err := ReplicateHandler(st, br, discardLog())(context.Background(), args); err != nil {
		t.Fatalf("handler success returned %v", err)
	}
	if br.gotDigest != "d0" || br.gotNode != target {
		t.Errorf("BrokerPull called with (%q,%s), want (d0,%s)", br.gotDigest, br.gotNode, target)
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != store.TaskStatusSuccess {
		t.Errorf("finalized = %+v, want one success", st.finalized)
	}
}

func TestReplicateHandlerPullFailureFailsTask(t *testing.T) {
	st := &replicateStoreSpy{}
	br := &brokerStub{err: errors.New("no holder")}
	args := mustJSON(t, ReplicateArgs{TaskID: uuid.New(), Digest: "d0", TargetNodeID: uuid.New()})

	err := ReplicateHandler(st, br, discardLog())(context.Background(), args)
	if err == nil {
		t.Fatalf("handler returned nil on pull failure, want the cause (for dispatcher retry)")
	}
	if len(st.finalized) != 1 || st.finalized[0].Status != store.TaskStatusFailed {
		t.Errorf("finalized = %+v, want one failed", st.finalized)
	}
}

func TestReplicateHandlerAlreadyTerminalSkipsPull(t *testing.T) {
	st := &replicateStoreSpy{alreadyTerminal: true}
	br := &brokerStub{}
	args := mustJSON(t, ReplicateArgs{TaskID: uuid.New(), Digest: "d0", TargetNodeID: uuid.New()})

	if err := ReplicateHandler(st, br, discardLog())(context.Background(), args); err != nil {
		t.Fatalf("already-terminal returned %v, want nil", err)
	}
	if br.gotDigest != "" {
		t.Errorf("BrokerPull was called on already-terminal task")
	}
	if len(st.finalized) != 0 {
		t.Errorf("already-terminal finalized = %+v, want none", st.finalized)
	}
}
