// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/store"
)

// TestCommitCascadeWithNodeIntentAbortsOnSeveredIntent proves the node
// soft-delete finalize is conditional on the delete-intent still carrying this
// attempt's rev: if a reaper (or a racing second delete) severs and re-stamps the
// intent, the final guarded chunk loses the CAS, the node put is NOT applied, and
// the caller gets ErrConcurrentUpdate to retry - the node is never soft-deleted
// after the create-blocking guarantee has lapsed.
func TestCommitCascadeWithNodeIntentAbortsOnSeveredIntent(t *testing.T) {
	s, cli := FreshStore(t)
	ctx := context.Background()
	nodeID := uuid.New()
	intentKey := nodeDeletingKey(nodeID)

	myRev, err := s.setDeleteIntent(ctx, intentKey, time.Now())
	if err != nil {
		t.Fatalf("setDeleteIntent: %v", err)
	}
	// Sever and re-stamp the intent so its CreateRevision no longer equals myRev
	// (models a reaper sweep followed by a fresh operator retry).
	if _, err := cli.Raw().Delete(ctx, intentKey); err != nil {
		t.Fatalf("sever intent: %v", err)
	}
	if _, err := cli.Raw().Put(ctx, intentKey, "restamped"); err != nil {
		t.Fatalf("re-stamp intent: %v", err)
	}

	nodePutKey := nodeKey(nodeID)
	cascade := []clientv3.Op{
		clientv3.OpDelete(nodeNameGuard("finalguard")),
		clientv3.OpPut(nodePutKey, "sentinel-node-row"),
	}
	if err := s.commitCascadeWithNodeIntent(ctx, cascade, intentKey, myRev); !errors.Is(err, store.ErrConcurrentUpdate) {
		t.Fatalf("commitCascadeWithNodeIntent with severed intent = %v, want ErrConcurrentUpdate", err)
	}
	// The node put must NOT have landed.
	if _, found, err := cli.Get(ctx, nodePutKey); err != nil {
		t.Fatalf("get node key: %v", err)
	} else if found {
		t.Errorf("node row written despite severed intent, want absent (soft-delete must be blocked)")
	}
}

// TestCommitCascadeWithNodeIntentCommitsAndClearsIntent is the revert-confirming
// positive: with the intent still ours, the final chunk commits the node put and
// deletes the intent key in the same txn.
func TestCommitCascadeWithNodeIntentCommitsAndClearsIntent(t *testing.T) {
	s, cli := FreshStore(t)
	ctx := context.Background()
	nodeID := uuid.New()
	intentKey := nodeDeletingKey(nodeID)

	myRev, err := s.setDeleteIntent(ctx, intentKey, time.Now())
	if err != nil {
		t.Fatalf("setDeleteIntent: %v", err)
	}

	nodePutKey := nodeKey(nodeID)
	cascade := []clientv3.Op{
		clientv3.OpDelete(nodeNameGuard("finalguard-ok")),
		clientv3.OpPut(nodePutKey, "sentinel-node-row"),
	}
	if err := s.commitCascadeWithNodeIntent(ctx, cascade, intentKey, myRev); err != nil {
		t.Fatalf("commitCascadeWithNodeIntent = %v, want nil", err)
	}
	if _, found, err := cli.Get(ctx, nodePutKey); err != nil {
		t.Fatalf("get node key: %v", err)
	} else if !found {
		t.Errorf("node row not written, want present")
	}
	if _, found, err := cli.Get(ctx, intentKey); err != nil {
		t.Fatalf("get intent key: %v", err)
	} else if found {
		t.Errorf("intent key still present, want deleted by the finalize txn")
	}
}
