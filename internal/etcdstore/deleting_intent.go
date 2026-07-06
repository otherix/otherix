// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

// Delete-intent keys close the create-during-delete TOCTOU for infra resources
// (networks, storage pools, firmwares, nodes). An infra delete counts references
// then commits a delete; a reference created in that window would dangle on a
// deleted resource. etcd txn If cannot assert prefix-emptiness, and making every
// create parent-touch the infra row would serialize all VM creation on the
// default firmware / every heartbeat on the node row. Instead the delete writes
// a per-resource intent key that every reference-create checks (not writes):
//
//   - The delete SETS deletingKey FIRST, then counts references. After the intent
//     is set no new reference can commit, so the post-intent count is authoritative.
//   - Every reference-create adds If(CreateRevision(deletingKey)==0) to its commit.
//     This is a read-comparison on a normally-absent key, so it never serializes
//     creates against each other - only against a concurrent delete of that exact
//     resource.
//   - The delete FINALIZE guards on If(CreateRevision(deletingKey)==myRev) (the rev
//     it created) so the reaper (or a racing second delete) severing the intent
//     aborts the finalize instead of silently re-opening the window. Correctness is
//     therefore independent of the reaper cadence; the reaper only bounds the
//     block window on a crashed delete.
//
// The value is the delete-attempt timestamp (RFC3339), read only by the reaper.

func deletingKey(resource string, id uuid.UUID) string {
	return etcd.Key("deleting", resource, id.String())
}

func networkDeletingKey(id uuid.UUID) string     { return deletingKey("networks", id) }
func firmwareDeletingKey(id uuid.UUID) string    { return deletingKey("firmwares", id) }
func storagePoolDeletingKey(id uuid.UUID) string { return deletingKey("storage_pools", id) }

// setDeleteIntent creates the intent key for a delete attempt and returns its
// CreateRevision (the liveness token the finalize CASes on). If an intent already
// exists (a concurrent or prior crashed delete), it returns the existing key's
// CreateRevision so the caller races the same token; a stale one is swept by the
// reaper and a fresh operator retry re-stamps. now is passed in (the store has no
// wall clock of its own on the hot path); it is stored only for the reaper.
func (s *Store) setDeleteIntent(ctx context.Context, key string, now time.Time) (int64, error) {
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, now.UTC().Format(time.RFC3339))).
		Else(clientv3.OpGet(key)).
		Commit()
	if err != nil {
		return 0, fmt.Errorf("set delete intent %s: %v", key, err)
	}
	if resp.Succeeded {
		// We created it; its CreateRevision equals this txn's revision.
		return resp.Header.Revision, nil
	}
	// Lost the create: an intent already exists. Race the same token.
	if len(resp.Responses) == 1 {
		if kvs := resp.Responses[0].GetResponseRange().GetKvs(); len(kvs) == 1 {
			return kvs[0].CreateRevision, nil
		}
	}
	// The key vanished between the failed create and the Else read (reaper);
	// re-read to get a definitive rev.
	got, err := s.c.Raw().Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("read delete intent %s: %v", key, err)
	}
	if len(got.Kvs) == 0 {
		return 0, nil
	}
	return got.Kvs[0].CreateRevision, nil
}

// deleteIntentGuard is the finalize-time compare: the intent key still carries
// the CreateRevision this delete attempt created. A reaper sweep or a racing
// second delete that severed the intent flips this false, so the finalize aborts
// rather than deleting the row after the create-blocking guarantee has lapsed.
func deleteIntentGuard(key string, myRev int64) clientv3.Cmp {
	return clientv3.Compare(clientv3.CreateRevision(key), "=", myRev)
}

// clearDeleteIntent removes this attempt's intent key, guarded on its rev so a
// concurrent delete's freshly-stamped intent is never clobbered. Best-effort:
// used on the refuse (refs>0) path where a failure only leaks an intent the
// reaper will sweep.
func (s *Store) clearDeleteIntent(ctx context.Context, key string, myRev int64) {
	_, _ = s.c.Raw().Txn(ctx).
		If(deleteIntentGuard(key, myRev)).
		Then(clientv3.OpDelete(key)).
		Commit()
}

// deleteIntentPresent reports whether a delete-intent key exists - used by the
// create-side classifiers to distinguish a lost CAS caused by an in-flight
// delete from an ordinary name/MAC conflict.
func (s *Store) deleteIntentPresent(ctx context.Context, key string) (bool, error) {
	resp, err := s.c.Raw().Get(ctx, key, clientv3.WithCountOnly())
	if err != nil {
		return false, fmt.Errorf("probe delete intent %s: %v", key, err)
	}
	return resp.Count > 0, nil
}

// anyDeleteIntentPresent reports whether any of the given intent keys exists -
// the create-side classifiers use it to attribute a lost CAS to an in-flight
// infra delete (any referenced resource) rather than a name/MAC conflict.
func (s *Store) anyDeleteIntentPresent(ctx context.Context, keys ...string) (bool, error) {
	for _, k := range keys {
		present, err := s.deleteIntentPresent(ctx, k)
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

// DefaultDeleteIntentStaleAfter is the reaper's staleness threshold. With the
// finalize-CAS the reaper is a liveness bound only (never a correctness gap), so
// a generous interval is safer: it far exceeds any real delete's duration, so it
// never races a live delete, and only bounds how long a crashed delete's leaked
// intent blocks new references to that resource.
const DefaultDeleteIntentStaleAfter = 5 * time.Minute

// DeleteIntentReaperFunc is the periodic maintenance function that sweeps stale
// delete-intent keys (crashed deletes). Registered on the worker scheduler.
func DeleteIntentReaperFunc(st *Store, staleAfter time.Duration, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := st.ReapStaleDeleteIntents(ctx, time.Now().UTC(), staleAfter)
		if err != nil {
			return fmt.Errorf("reap stale delete intents: %v", err)
		}
		if n > 0 {
			log.InfoContext(ctx, "delete_intent.reap", "reaped", n)
		}
		return nil
	}
}

// ReapStaleDeleteIntents removes any delete-intent key older than staleAfter. A
// crashed delete leaks its intent, which blocks new references to that resource;
// with the finalize-CAS this is a liveness bound only (never a correctness gap).
// Called periodically by the worker maintenance scheduler; idempotent and safe to
// run on every HA replica. now is passed in.
func (s *Store) ReapStaleDeleteIntents(ctx context.Context, now time.Time, staleAfter time.Duration) (int, error) {
	resp, err := s.c.Raw().Get(ctx, etcd.Key("deleting")+"/", clientv3.WithPrefix())
	if err != nil {
		return 0, fmt.Errorf("range delete intents: %v", err)
	}
	cutoff := now.UTC().Add(-staleAfter)
	reaped := 0
	for _, kv := range resp.Kvs {
		ts, perr := time.Parse(time.RFC3339, string(kv.Value))
		if perr != nil {
			// Corrupt value: treat as stale (a delete never writes a bad value).
			ts = time.Time{}
		}
		if ts.After(cutoff) {
			continue
		}
		// Delete only if unchanged since we read it, so a delete that re-stamped
		// concurrently is not clobbered.
		if _, derr := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(string(kv.Key)), "=", kv.ModRevision)).
			Then(clientv3.OpDelete(string(kv.Key))).
			Commit(); derr != nil {
			return reaped, fmt.Errorf("reap delete intent %s: %v", kv.Key, derr)
		}
		reaped++
	}
	return reaped, nil
}
