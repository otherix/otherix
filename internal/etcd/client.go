// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3client"
)

// KeyPrefix roots every Otherix key so the keyspace is namespaced and a single
// range scan can sweep all control-plane state (backup / debug / migration).
const KeyPrefix = "/otherix/"

// MaxValueBytes caps a single value below etcd's default 1.5 MiB request limit,
// leaving headroom for the key plus transaction framing. The API edge enforces
// tighter per-field maxima (ADR 0030 #1); this is the backstop the KV layer
// refuses to cross.
const MaxValueBytes = 1024 * 1024

// ErrValueTooLarge is returned by the write helpers when a marshalled value
// exceeds MaxValueBytes.
var ErrValueTooLarge = errors.New("etcd: value exceeds max size")

// Client is a thin KV facade over an etcd clientv3 connection: namespaced
// keys, a value size guard, JSON codec, prefix ranges, and a watch passthrough.
// Higher layers (the etcd-backed store) build multi-key compare-and-set
// transactions through Raw.
type Client struct {
	c *clientv3.Client
}

// NewClient builds an in-process KV client over the embedded member - no
// network hop, the co-located control-plane access path. Closing it does not
// stop the member (use Runtime.Stop for that).
func NewClient(r *Runtime) *Client {
	return &Client{c: v3client.New(r.Etcd().Server)}
}

// NewClientWithRaw wraps an existing clientv3.Client (external endpoints,
// tests). The Client takes ownership: Close closes the wrapped client.
func NewClientWithRaw(c *clientv3.Client) *Client {
	return &Client{c: c}
}

// Raw exposes the underlying clientv3.Client so the store layer can build
// transactions (Txn/If/Then/Else) and complex ranges the facade does not wrap.
func (c *Client) Raw() *clientv3.Client { return c.c }

// Close releases the wrapped client.
func (c *Client) Close() error { return c.c.Close() }

// Key joins parts under the Otherix prefix: Key("vms", id) -> "/otherix/vms/<id>".
func Key(parts ...string) string {
	return KeyPrefix + strings.Join(parts, "/")
}

// capValue rejects a value that would exceed the size backstop.
func capValue(b []byte) error {
	if len(b) > MaxValueBytes {
		return fmt.Errorf("%w: %d > %d bytes", ErrValueTooLarge, len(b), MaxValueBytes)
	}
	return nil
}

// Marshal JSON-encodes v and enforces the size cap, returning the bytes a
// caller can hand to a raw transaction (clientv3.OpPut) while keeping the same
// size guarantee the Put helpers provide.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal value: %v", err)
	}
	if err := capValue(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Put writes raw bytes at key, enforcing the size cap.
func (c *Client) Put(ctx context.Context, key string, value []byte) error {
	if err := capValue(value); err != nil {
		return err
	}
	if _, err := c.c.Put(ctx, key, string(value)); err != nil {
		return fmt.Errorf("put %q: %v", key, err)
	}
	return nil
}

// PutJSON marshals v to JSON and writes it at key, enforcing the size cap.
func (c *Client) PutJSON(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal value for %q: %v", key, err)
	}
	return c.Put(ctx, key, b)
}

// GetJSON reads key and unmarshals the value into v. found is false (with a nil
// error) when the key is absent, so callers map absence to their own
// not-found sentinel.
func (c *Client) GetJSON(ctx context.Context, key string, v any) (found bool, err error) {
	resp, err := c.c.Get(ctx, key)
	if err != nil {
		return false, fmt.Errorf("get %q: %v", key, err)
	}
	if len(resp.Kvs) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(resp.Kvs[0].Value, v); err != nil {
		return false, fmt.Errorf("unmarshal value for %q: %v", key, err)
	}
	return true, nil
}

// Delete removes key. deleted reports whether a key was actually present.
func (c *Client) Delete(ctx context.Context, key string) (deleted bool, err error) {
	resp, err := c.c.Delete(ctx, key)
	if err != nil {
		return false, fmt.Errorf("delete %q: %v", key, err)
	}
	return resp.Deleted > 0, nil
}

// PutIfAbsent writes key=value only when key does not already exist, in a
// single transaction. created is false (with a nil error) when the key was
// already present - the building block for uniqueness guard keys that replace
// the partial unique indexes the SQL schema used.
func (c *Client) PutIfAbsent(ctx context.Context, key string, value []byte) (created bool, err error) {
	if err := capValue(value); err != nil {
		return false, err
	}
	resp, err := c.c.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, string(value))).
		Commit()
	if err != nil {
		return false, fmt.Errorf("put-if-absent %q: %v", key, err)
	}
	return resp.Succeeded, nil
}

// KV is a decoded key/value pair returned by the range helpers.
type KV struct {
	Key   string
	Value []byte
}

// Range returns every key/value under prefix, sorted by key ascending. Intended
// for bounded collections; hot-path filtered lists use secondary-index prefixes
// plus RangePaged, never an ad-hoc scan (ADR 0030 #7).
func (c *Client) Range(ctx context.Context, prefix string) ([]KV, error) {
	resp, err := c.c.Get(ctx, prefix,
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, fmt.Errorf("range %q: %v", prefix, err)
	}
	return toKVs(resp), nil
}

// RangePaged returns up to limit key/value pairs under prefix whose key sorts
// strictly after `after` (pass "" for the first page), ascending. more reports
// whether further keys remain past the returned page - the basis for cursor
// pagination over secondary-index prefixes.
func (c *Client) RangePaged(ctx context.Context, prefix, after string, limit int64) (items []KV, more bool, err error) {
	start := prefix
	if after != "" {
		start = after + "\x00" // exclusive lower bound: the next possible key after `after`
	}
	resp, err := c.c.Get(ctx, start,
		clientv3.WithRange(clientv3.GetPrefixRangeEnd(prefix)),
		clientv3.WithLimit(limit),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, false, fmt.Errorf("range-paged %q: %v", prefix, err)
	}
	return toKVs(resp), resp.More, nil
}

// Watch returns the change stream for every key under prefix. The watch runs
// until ctx is cancelled; the queue (Phase 3 slice 12) drives its worker loop
// off this.
func (c *Client) Watch(ctx context.Context, prefix string) clientv3.WatchChan {
	return c.c.Watch(ctx, prefix, clientv3.WithPrefix())
}

func toKVs(resp *clientv3.GetResponse) []KV {
	if len(resp.Kvs) == 0 {
		return nil
	}
	out := make([]KV, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		out = append(out, KV{Key: string(kv.Key), Value: kv.Value})
	}
	return out
}
