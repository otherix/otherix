// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestPullSagaLifecycle(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	consumer, holder := uuid.New(), uuid.New()
	digest := "00000000000000000000000000000000000000000000000000000000000000ef"

	saga, plaintext, err := s.CreatePullSaga(ctx, store.CreatePullSagaParams{
		ID:           uuid.New(),
		Digest:       digest,
		ConsumerNode: consumer,
		HolderNode:   holder,
		TokenTTL:     time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePullSaga: %v", err)
	}
	if plaintext == "" {
		t.Fatalf("CreatePullSaga returned empty token plaintext")
	}
	got, err := s.PullSagaByID(ctx, saga.ID)
	if err != nil || got.Digest != digest || got.ConsumerNode != consumer {
		t.Fatalf("PullSagaByID = %+v, %v", got, err)
	}
	if err := s.ConsumePullToken(ctx, plaintext, digest, consumer); err != nil {
		t.Fatalf("first ConsumePullToken: %v", err)
	}
	if err := s.ConsumePullToken(ctx, plaintext, digest, consumer); err == nil {
		t.Errorf("replayed ConsumePullToken = nil, want rejection")
	}
}

func TestPullTokenWrongBinding(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	consumer, holder := uuid.New(), uuid.New()
	digest := "00000000000000000000000000000000000000000000000000000000000000aa"

	_, plaintext, err := s.CreatePullSaga(ctx, store.CreatePullSagaParams{
		ID: uuid.New(), Digest: digest, ConsumerNode: consumer, HolderNode: holder, TokenTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePullSaga: %v", err)
	}
	if err := s.ConsumePullToken(ctx, plaintext, "00000000000000000000000000000000000000000000000000000000000000bb", consumer); err == nil {
		t.Errorf("wrong-digest ConsumePullToken = nil, want rejection")
	}
	if err := s.ConsumePullToken(ctx, plaintext, digest, uuid.New()); err == nil {
		t.Errorf("wrong-consumer ConsumePullToken = nil, want rejection")
	}
}

func TestPullTokenTTL(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	consumer, holder := uuid.New(), uuid.New()
	digest := "00000000000000000000000000000000000000000000000000000000000000cc"

	_, plaintext, err := s.CreatePullSaga(ctx, store.CreatePullSagaParams{
		ID: uuid.New(), Digest: digest, ConsumerNode: consumer, HolderNode: holder,
		TokenTTL: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("CreatePullSaga: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.ConsumePullToken(ctx, plaintext, digest, consumer); err == nil {
		t.Errorf("expired ConsumePullToken = nil, want rejection")
	}
}
