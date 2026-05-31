// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command driver is a throwaway harness for the slice-9 HA multi-process smoke.
// It drives the membership steps the product orchestration does not yet wire
// (AddLearner / PromoteMember) by reusing the production internal/etcd
// primitives against a member's loopback client endpoint, plus put/get for
// replication checks. Not built into any binary; invoked by dev/smoke/ha/run.sh.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "driver:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: driver SUBCOMMAND (add-learner|promote|voters|wait-serving|put|get)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	switch args[0] {
	case "add-learner": // add-learner <clientURL> <peerURL> -> prints member id (decimal)
		id, err := etcd.AddLearner(ctx, args[1], args[2], log)
		if err != nil {
			return err
		}
		fmt.Println(strconv.FormatUint(id, 10))
		return nil
	case "promote": // promote <clientURL> <memberID-decimal>
		id, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			return err
		}
		return etcd.PromoteMember(ctx, args[1], id, log)
	case "wait-serving": // wait-serving <clientURL>
		return etcd.WaitMemberServing(ctx, args[1])
	case "voters": // voters <clientURL> -> prints count
		n, err := etcd.VoterCount(ctx, args[1])
		if err != nil {
			return err
		}
		fmt.Println(n)
		return nil
	case "put": // put <clientURL> <key> <val>
		return kv(args[1], func(cli *clientv3.Client) error {
			_, err := cli.Put(ctx, args[2], args[3])
			return err
		})
	case "get": // get <clientURL> <key> -> prints value (errors if absent)
		return kv(args[1], func(cli *clientv3.Client) error {
			resp, err := cli.Get(ctx, args[2])
			if err != nil {
				return err
			}
			if len(resp.Kvs) == 0 {
				return fmt.Errorf("key %q not found", args[2])
			}
			fmt.Println(string(resp.Kvs[0].Value))
			return nil
		})
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func kv(clientURL string, fn func(*clientv3.Client) error) error {
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{clientURL}, DialTimeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	return fn(cli)
}
