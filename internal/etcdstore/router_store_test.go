// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	apipkg "github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/etcdstore"
)

// The milestone assertion: the etcd store satisfies the entire api-server router
// contract (the union of every handler Store + idempotency + readiness pinger +
// agent-cert lookup), so cmd/api can build the whole server against it.
var _ apipkg.RouterStore = (*etcdstore.Store)(nil)
