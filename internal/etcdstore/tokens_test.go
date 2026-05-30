// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	apitokenshandlers "github.com/otherix/otherix/internal/api/handlers/apitokens"
	clusterhandlers "github.com/otherix/otherix/internal/api/handlers/cluster"
	jointokenshandlers "github.com/otherix/otherix/internal/api/handlers/jointokens"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the api-token, join-token, and cluster handler
// contracts (all queue-independent).
var (
	_ apitokenshandlers.Store  = (*etcdstore.Store)(nil)
	_ jointokenshandlers.Store = (*etcdstore.Store)(nil)
	_ clusterhandlers.Store    = (*etcdstore.Store)(nil)
)

func TestAPITokenLifecycle(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	user := uuid.New()
	exp := time.Now().UTC().Add(24 * time.Hour)
	tok, err := s.CreateAPIToken(ctx, store.CreateApiTokenParams{
		ID: uuid.New(), UserID: user, Name: "ci", TokenHash: []byte("hash-1"), Prefix: "otx_abcd", ExpiresAt: &exp,
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if tok.CreatedAt.IsZero() {
		t.Errorf("CreateAPIToken created_at not stamped")
	}
	got, err := s.APITokenByID(ctx, tok.ID)
	if err != nil || got.Name != "ci" {
		t.Fatalf("APITokenByID = (%+v, %v)", got, err)
	}
	// Second token, then list (revoked excluded by default).
	tok2, err := s.CreateAPIToken(ctx, store.CreateApiTokenParams{ID: uuid.New(), UserID: user, Name: "tmp", TokenHash: []byte("hash-2"), Prefix: "otx_efgh"})
	if err != nil {
		t.Fatalf("CreateAPIToken 2: %v", err)
	}
	if err := s.RevokeAPIToken(ctx, tok2.ID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	active, err := s.ListAPITokensByUser(ctx, store.ListApiTokensByUserParams{UserID: user, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListAPITokensByUser: %v", err)
	}
	if len(active) != 1 || active[0].ID != tok.ID {
		t.Errorf("active tokens = %v, want [%v]", active, tok.ID)
	}
	withRevoked, err := s.ListAPITokensByUser(ctx, store.ListApiTokensByUserParams{UserID: user, IncludeRevoked: true, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListAPITokensByUser(incl): %v", err)
	}
	if len(withRevoked) != 2 {
		t.Errorf("with revoked = %d, want 2", len(withRevoked))
	}
	// Revoke is idempotent; missing token is silent.
	if err := s.RevokeAPIToken(ctx, tok2.ID); err != nil {
		t.Errorf("RevokeAPIToken(idempotent): %v", err)
	}
	if err := s.RevokeAPIToken(ctx, uuid.New()); err != nil {
		t.Errorf("RevokeAPIToken(missing): %v", err)
	}
	if _, err := s.APITokenByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("APITokenByID(absent) = %v, want store.ErrNotFound", err)
	}
}

func TestJoinTokenLifecycle(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	exp := time.Now().UTC().Add(time.Hour)
	jt, err := s.CreateJoinToken(ctx, store.CreateJoinTokenParams{
		ID: uuid.New(), TokenHash: []byte("jhash-1"), ExpiresAt: exp,
	})
	if err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	byID, err := s.JoinTokenByID(ctx, jt.ID)
	if err != nil || byID.ID != jt.ID {
		t.Fatalf("JoinTokenByID = (%+v, %v)", byID, err)
	}
	byHash, err := s.JoinTokenByHash(ctx, []byte("jhash-1"))
	if err != nil || byHash.ID != jt.ID {
		t.Fatalf("JoinTokenByHash = (%+v, %v)", byHash, err)
	}
	// List (active), with a seeded consumption row -> count 1.
	cons := store.JoinTokenConsumption{ID: uuid.New(), JoinTokenID: jt.ID, ConsumedAt: time.Now().UTC()}
	if err := cli.PutJSON(ctx, etcd.Key("join_token_consumptions", cons.ID.String()), cons); err != nil {
		t.Fatalf("seed consumption: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "join_token_consumptions", "token", jt.ID.String(), cons.ID.String()), []byte(cons.ID.String())); err != nil {
		t.Fatalf("seed consumption index: %v", err)
	}
	list, err := s.ListJoinTokens(ctx, store.ListJoinTokensParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
	if len(list) != 1 || list[0].ConsumptionCount != 1 {
		t.Errorf("ListJoinTokens = %+v, want one row with consumption_count 1", list)
	}
	consList, err := s.ListJoinTokenConsumptions(ctx, store.ListJoinTokenConsumptionsParams{JoinTokenID: jt.ID, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListJoinTokenConsumptions: %v", err)
	}
	if len(consList) != 1 || consList[0].ID != cons.ID {
		t.Errorf("consumptions = %v, want [%v]", consList, cons.ID)
	}
	// Revoke clamps expiry; the token then drops from the default (active) list.
	if err := s.RevokeJoinToken(ctx, jt.ID); err != nil {
		t.Fatalf("RevokeJoinToken: %v", err)
	}
	after, err := s.ListJoinTokens(ctx, store.ListJoinTokensParams{LimitCount: 200})
	if err != nil {
		t.Fatalf("ListJoinTokens after revoke: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("active join tokens after revoke = %d, want 0", len(after))
	}
	inclExpired, err := s.ListJoinTokens(ctx, store.ListJoinTokensParams{IncludeExpired: true, LimitCount: 200})
	if err != nil {
		t.Fatalf("ListJoinTokens(incl expired): %v", err)
	}
	if len(inclExpired) != 1 {
		t.Errorf("incl expired = %d, want 1", len(inclExpired))
	}
}
