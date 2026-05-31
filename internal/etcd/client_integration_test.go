// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/etcd"
)

// startTestClient spins up a single-node embedded member and an in-process KV
// client over it, registering cleanup. Shared by the client integration tests.
func startTestClient(t *testing.T) *etcd.Client {
	t.Helper()
	cfg := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n1",
		DataDir:      filepath.Join(t.TempDir(), "member"),
		PeerURL:      fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClientURL:    fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
		ClusterToken: "otherix-test",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := etcd.Start(ctx, cfg, log)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cli := etcd.NewClient(r)
	t.Cleanup(func() {
		_ = cli.Close()
		r.Stop(10 * time.Second)
	})
	return cli
}

type rec struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func TestClientJSONRoundtrip(t *testing.T) {
	cli := startTestClient(t)
	ctx := context.Background()
	key := etcd.Key("test", "rt")

	want := rec{Name: "alpha", N: 7}
	if err := cli.PutJSON(ctx, key, want); err != nil {
		t.Fatalf("PutJSON: %v", err)
	}
	var got rec
	found, err := cli.GetJSON(ctx, key, &got)
	if err != nil || !found {
		t.Fatalf("GetJSON = (found=%v, %v), want (true, nil)", found, err)
	}
	if got != want {
		t.Errorf("GetJSON value = %+v, want %+v", got, want)
	}

	// Absent key -> found=false, nil error.
	found, err = cli.GetJSON(ctx, etcd.Key("test", "absent"), &got)
	if err != nil || found {
		t.Errorf("GetJSON(absent) = (found=%v, %v), want (false, nil)", found, err)
	}

	// Delete -> deleted=true, then absent.
	deleted, err := cli.Delete(ctx, key)
	if err != nil || !deleted {
		t.Errorf("Delete = (deleted=%v, %v), want (true, nil)", deleted, err)
	}
	if found, _ := cli.GetJSON(ctx, key, &got); found {
		t.Errorf("key still present after Delete")
	}
}

func TestClientPutOversizeRejected(t *testing.T) {
	cli := startTestClient(t)
	err := cli.Put(context.Background(), etcd.Key("test", "big"), bytes.Repeat([]byte{'x'}, etcd.MaxValueBytes+1))
	if err == nil {
		t.Fatal("Put(oversize) = nil, want ErrValueTooLarge")
	}
}

func TestClientPutIfAbsent(t *testing.T) {
	cli := startTestClient(t)
	ctx := context.Background()
	key := etcd.Key("uniq", "email", "a@example.test")

	created, err := cli.PutIfAbsent(ctx, key, []byte("id-1"))
	if err != nil || !created {
		t.Fatalf("first PutIfAbsent = (created=%v, %v), want (true, nil)", created, err)
	}
	created, err = cli.PutIfAbsent(ctx, key, []byte("id-2"))
	if err != nil || created {
		t.Fatalf("second PutIfAbsent = (created=%v, %v), want (false, nil)", created, err)
	}
	// The guard value must be the first writer's, unchanged.
	var raw []byte
	if items, err := cli.Range(ctx, key); err != nil || len(items) != 1 {
		t.Fatalf("Range(guard) = (%v, %v), want 1 item", items, err)
	} else {
		raw = items[0].Value
	}
	if string(raw) != "id-1" {
		t.Errorf("guard value = %q, want id-1 (first writer wins)", raw)
	}
}

func TestClientRangePaged(t *testing.T) {
	cli := startTestClient(t)
	ctx := context.Background()
	prefix := etcd.Key("index", "paged") + "/"
	for _, k := range []string{"a", "b", "c", "d"} {
		if err := cli.Put(ctx, prefix+k, []byte(k)); err != nil {
			t.Fatalf("seed Put %q: %v", k, err)
		}
	}

	first, more, err := cli.RangePaged(ctx, prefix, "", 2)
	if err != nil {
		t.Fatalf("RangePaged page 1: %v", err)
	}
	if !more || len(first) != 2 || string(first[0].Value) != "a" || string(first[1].Value) != "b" {
		t.Fatalf("page 1 = %v (more=%v), want [a b] more=true", kvVals(first), more)
	}

	second, more, err := cli.RangePaged(ctx, prefix, first[1].Key, 2)
	if err != nil {
		t.Fatalf("RangePaged page 2: %v", err)
	}
	if more || len(second) != 2 || string(second[0].Value) != "c" || string(second[1].Value) != "d" {
		t.Fatalf("page 2 = %v (more=%v), want [c d] more=false", kvVals(second), more)
	}
}

func TestClientWatch(t *testing.T) {
	cli := startTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prefix := etcd.Key("watch", "topic") + "/"

	ch := cli.Watch(ctx, prefix)
	if err := cli.Put(ctx, prefix+"job1", []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	select {
	case wr := <-ch:
		if len(wr.Events) == 0 {
			t.Fatalf("watch response had no events")
		}
		if got := string(wr.Events[0].Kv.Value); got != "payload" {
			t.Errorf("watch event value = %q, want payload", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}

func kvVals(items []etcd.KV) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it.Value))
	}
	return out
}
